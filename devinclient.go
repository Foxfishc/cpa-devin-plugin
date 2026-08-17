package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	devinproto "cpa-devin-plugin/internal/devinproto"
	"cpa-devin-plugin/internal/devinproto/devinprotoconnect"
)

// Seat management procedure paths.
//
// The vendored descriptor set is a flattened single-package proto, so generated
// service names are rewritten (exa.api_server_pb.ExaSeatManagementPb_*). The real
// endpoints keep their original package, so these RPCs are addressed explicitly.
// Message wire formats are unaffected because protobuf encodes field numbers only.
const (
	procedureRegisterUser             = "/exa.seat_management_pb.SeatManagementService/RegisterUser"
	procedureMigrateAPIKey            = "/exa.seat_management_pb.SeatManagementService/MigrateApiKey"
	procedureGetSelfDevinSessionToken = "/exa.seat_management_pb.SeatManagementService/GetSelfDevinSessionToken"
	procedureGetUserStatus            = "/exa.seat_management_pb.SeatManagementService/GetUserStatus"
)

// deviceFingerprintLength is the fingerprint length reported in the metadata `f` field.
const deviceFingerprintLength = 366

// devinClient bundles the Devin RPC clients used by the plugin.
type devinClient struct {
	cfg   pluginConfig
	token string

	apiServer devinprotoconnect.ApiServerServiceClient

	registerUser      *connect.Client[devinproto.ExaSeatManagementPb_RegisterUserRequest, devinproto.ExaSeatManagementPb_RegisterUserResponse]
	migrateAPIKey     *connect.Client[devinproto.ExaSeatManagementPb_MigrateApiKeyRequest, devinproto.ExaSeatManagementPb_MigrateApiKeyResponse]
	selfSessionToken  *connect.Client[devinproto.ExaSeatManagementPb_GetSelfDevinSessionTokenRequest, devinproto.ExaSeatManagementPb_GetSelfDevinSessionTokenResponse]
	userStatusRequest *connect.Client[devinproto.ExaSeatManagementPb_GetUserStatusRequest, devinproto.ExaSeatManagementPb_GetUserStatusResponse]
}

// newDevinClient builds Devin RPC clients bound to the host HTTP bridge.
func newDevinClient(cfg pluginConfig, token string) *devinClient {
	httpClient := hostHTTPClient{}
	base := strings.TrimRight(cfg.BaseURL, "/")
	return &devinClient{
		cfg:               cfg,
		token:             strings.TrimSpace(token),
		apiServer:         devinprotoconnect.NewApiServerServiceClient(httpClient, base),
		registerUser:      connect.NewClient[devinproto.ExaSeatManagementPb_RegisterUserRequest, devinproto.ExaSeatManagementPb_RegisterUserResponse](httpClient, base+procedureRegisterUser),
		migrateAPIKey:     connect.NewClient[devinproto.ExaSeatManagementPb_MigrateApiKeyRequest, devinproto.ExaSeatManagementPb_MigrateApiKeyResponse](httpClient, base+procedureMigrateAPIKey),
		selfSessionToken:  connect.NewClient[devinproto.ExaSeatManagementPb_GetSelfDevinSessionTokenRequest, devinproto.ExaSeatManagementPb_GetSelfDevinSessionTokenResponse](httpClient, base+procedureGetSelfDevinSessionToken),
		userStatusRequest: connect.NewClient[devinproto.ExaSeatManagementPb_GetUserStatusRequest, devinproto.ExaSeatManagementPb_GetUserStatusResponse](httpClient, base+procedureGetUserStatus),
	}
}

// applyBasicAuth sets the duplicated-token Basic header used by the Devin API server.
func (c *devinClient) applyBasicAuth(header http.Header) {
	if c == nil || c.token == "" {
		return
	}
	header.Set("Authorization", "Basic "+c.token+"-"+c.token)
}

// applyBearerAuth sets the bearer header used by the seat management service.
func (c *devinClient) applyBearerAuth(header http.Header) {
	if c == nil || c.token == "" {
		return
	}
	header.Set("Authorization", "Bearer "+c.token)
}

// metadata builds the client metadata block expected by Devin RPCs.
func (c *devinClient) metadata() *devinproto.ExaCodeiumCommonPb_Metadata {
	fingerprint, errFingerprint := randomHex(deviceFingerprintLength)
	if errFingerprint != nil {
		fingerprint = strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	return &devinproto.ExaCodeiumCommonPb_Metadata{
		ApiKey:           proto.String(c.token),
		ExtensionName:    proto.String(c.cfg.ClientName),
		ExtensionVersion: proto.String(c.cfg.ClientVersion),
		IdeName:          proto.String(c.cfg.ClientName),
		IdeVersion:       proto.String(c.cfg.ClientVersion),
		Locale:           proto.String(c.cfg.Locale),
		Os:               proto.String(c.cfg.OS),
		F:                proto.String(fingerprint),
	}
}

// exchangeOneTimeToken converts a short-lived browser auth token into a durable
// Devin session token. RegisterUser returns a long-lived API key, which
// MigrateApiKey upgrades to a devin-session-token when the account supports it.
func (c *devinClient) exchangeOneTimeToken(ctx context.Context, oneTimeToken string) (string, string, error) {
	oneTimeToken = strings.TrimSpace(oneTimeToken)
	if oneTimeToken == "" {
		return "", "", errors.New("devin: auth token is empty")
	}
	// A pasted session token needs no exchange.
	if strings.HasPrefix(oneTimeToken, sessionTokenPrefix) {
		return oneTimeToken, "", nil
	}
	req := connect.NewRequest(&devinproto.ExaSeatManagementPb_RegisterUserRequest{
		FirebaseIdToken: proto.String(oneTimeToken),
	})
	resp, errCall := c.registerUser.CallUnary(ctx, req)
	if errCall != nil {
		return "", "", fmt.Errorf("devin: register user: %w", normalizeConnectError(errCall))
	}
	apiKey := strings.TrimSpace(resp.Msg.GetApiKey())
	if apiKey == "" {
		return "", "", errors.New("devin: register user returned no api key")
	}
	sessionToken, errMigrate := c.migrateToSessionToken(ctx, apiKey)
	if errMigrate != nil {
		// The API key itself is still usable as a credential.
		hostLog("warn", "devin: session token migration failed, keeping api key", map[string]any{"error": errMigrate.Error()})
		return apiKey, apiKey, nil
	}
	return sessionToken, apiKey, nil
}

// migrateToSessionToken upgrades an API key to a Devin session token.
func (c *devinClient) migrateToSessionToken(ctx context.Context, apiKey string) (string, error) {
	req := connect.NewRequest(&devinproto.ExaSeatManagementPb_MigrateApiKeyRequest{
		ApiKey: proto.String(apiKey),
	})
	resp, errCall := c.migrateAPIKey.CallUnary(ctx, req)
	if errCall != nil {
		return "", normalizeConnectError(errCall)
	}
	sessionToken := strings.TrimSpace(resp.Msg.GetSessionToken())
	if sessionToken == "" {
		return "", errors.New("devin: migrate api key returned no session token")
	}
	return sessionToken, nil
}

// refreshSessionToken mints a fresh session token for the current credential.
func (c *devinClient) refreshSessionToken(ctx context.Context) (string, error) {
	req := connect.NewRequest(&devinproto.ExaSeatManagementPb_GetSelfDevinSessionTokenRequest{
		Metadata: c.metadata(),
	})
	c.applyBearerAuth(req.Header())
	resp, errCall := c.selfSessionToken.CallUnary(ctx, req)
	if errCall != nil {
		return "", normalizeConnectError(errCall)
	}
	sessionToken := strings.TrimSpace(resp.Msg.GetSessionToken())
	if sessionToken == "" {
		return "", errors.New("devin: refresh returned no session token")
	}
	return sessionToken, nil
}

// userLabel validates the credential and returns a display label for it.
func (c *devinClient) userLabel(ctx context.Context) (string, error) {
	req := connect.NewRequest(&devinproto.ExaSeatManagementPb_GetUserStatusRequest{
		Metadata: c.metadata(),
	})
	c.applyBearerAuth(req.Header())
	resp, errCall := c.userStatusRequest.CallUnary(ctx, req)
	if errCall != nil {
		return "", normalizeConnectError(errCall)
	}
	if status := resp.Msg.GetUserStatus(); status != nil {
		if name := strings.TrimSpace(status.GetName()); name != "" {
			return name, nil
		}
	}
	return "", nil
}

// randomHex returns a lowercase hex string of the requested length.
func randomHex(length int) (string, error) {
	if length <= 0 {
		return "", nil
	}
	buf := make([]byte, (length+1)/2)
	if _, errRead := rand.Read(buf); errRead != nil {
		return "", errRead
	}
	return hex.EncodeToString(buf)[:length], nil
}

// normalizeConnectError unwraps a Connect error into a readable message.
func normalizeConnectError(err error) error {
	if err == nil {
		return nil
	}
	var connectErr *connect.Error
	if errors.As(err, &connectErr) {
		return fmt.Errorf("%s: %s", connectErr.Code().String(), connectErr.Message())
	}
	return err
}
