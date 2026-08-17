package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Plugin-owned command-line flags.
const (
	flagLogin  = "devin-login"
	flagImport = "devin-import"
	flagToken  = "devin-token"
)

// handleCommandLineRegister declares the plugin command-line flags.
func handleCommandLineRegister() ([]byte, error) {
	return okEnvelope(pluginapi.CommandLineRegistrationResponse{
		Flags: []pluginapi.CommandLineFlag{
			{Name: flagLogin, Usage: "Login to Devin Desktop in the browser and paste the displayed authentication token", Type: "bool"},
			{Name: flagImport, Usage: "Import a Devin credential from a locally installed Devin Desktop", Type: "bool"},
			{Name: flagToken, Usage: "Register a Devin credential from an authentication token without prompting", Type: "string"},
		},
	})
}

// handleCommandLineExecute runs the triggered plugin command.
func handleCommandLineExecute(raw []byte) ([]byte, error) {
	var req pluginapi.CommandLineExecutionRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	cfg := loadedConfig()
	if !cfg.Enabled {
		return okEnvelope(pluginapi.CommandLineExecutionResponse{
			Stderr:   []byte("devin: plugin is disabled in configuration\n"),
			ExitCode: 1,
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	switch {
	case flagProvided(req.TriggeredFlags, flagToken):
		return runTokenCommand(ctx, cfg, strings.TrimSpace(req.TriggeredFlags[flagToken].Value))
	case flagTrue(req.TriggeredFlags, flagImport):
		return runImportCommand(ctx, cfg)
	case flagTrue(req.TriggeredFlags, flagLogin):
		return runLoginCommand(ctx, cfg, flagTrue(req.Flags, "no-browser"))
	default:
		return okEnvelope(pluginapi.CommandLineExecutionResponse{})
	}
}

// runLoginCommand performs the interactive browser login.
func runLoginCommand(ctx context.Context, cfg pluginConfig, noBrowser bool) ([]byte, error) {
	var out strings.Builder
	out.WriteString("Devin Desktop login\n")
	out.WriteString("\n1. Open this page and sign in:\n   " + cfg.LoginURL + "\n")
	out.WriteString("2. Copy the authentication token shown on the page (it expires in 5 minutes).\n")
	out.WriteString("3. Paste it below and press Enter.\n\n")
	fmt.Print(out.String())
	out.Reset()

	if !noBrowser {
		if errOpen := openBrowser(cfg.LoginURL); errOpen != nil {
			fmt.Printf("Could not open a browser automatically: %v\n", errOpen)
		}
	}

	fmt.Print("Authentication token: ")
	token, errRead := readLine()
	if errRead != nil {
		return okEnvelope(pluginapi.CommandLineExecutionResponse{
			Stderr:   []byte(fmt.Sprintf("devin: failed to read token: %v\n", errRead)),
			ExitCode: 1,
		})
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return okEnvelope(pluginapi.CommandLineExecutionResponse{
			Stderr:   []byte("devin: no authentication token provided\n"),
			ExitCode: 1,
		})
	}
	return completeLogin(ctx, cfg, token, sourceBrowser)
}

// runTokenCommand registers a credential from a token supplied on the command line.
func runTokenCommand(ctx context.Context, cfg pluginConfig, token string) ([]byte, error) {
	if token == "" {
		return okEnvelope(pluginapi.CommandLineExecutionResponse{
			Stderr:   []byte("devin: --" + flagToken + " requires a value\n"),
			ExitCode: 1,
		})
	}
	return completeLogin(ctx, cfg, token, sourceManual)
}

// runImportCommand imports a credential from a local Devin Desktop install.
func runImportCommand(ctx context.Context, cfg pluginConfig) ([]byte, error) {
	storage, errImport := importFromDesktop(ctx, cfg)
	if errImport != nil {
		return okEnvelope(pluginapi.CommandLineExecutionResponse{
			Stderr:   []byte(errImport.Error() + "\n"),
			ExitCode: 1,
		})
	}
	return persistCredential(storage)
}

// completeLogin exchanges a token and returns the credential for the host to save.
func completeLogin(ctx context.Context, cfg pluginConfig, token, source string) ([]byte, error) {
	storage, errLogin := loginWithOneTimeToken(ctx, cfg, token, source)
	if errLogin != nil {
		return okEnvelope(pluginapi.CommandLineExecutionResponse{
			Stderr:   []byte(errLogin.Error() + "\n"),
			ExitCode: 1,
		})
	}
	return persistCredential(storage)
}

// persistCredential hands a new credential to the host for persistence.
func persistCredential(storage devinStorage) ([]byte, error) {
	authData, errBuild := authDataFromNewStorage(storage)
	if errBuild != nil {
		return okEnvelope(pluginapi.CommandLineExecutionResponse{
			Stderr:   []byte(errBuild.Error() + "\n"),
			ExitCode: 1,
		})
	}
	label := storage.Email
	if strings.TrimSpace(label) == "" {
		label = "Devin Desktop"
	}
	message := fmt.Sprintf("Devin credential saved as %s (%s)\n", authData.FileName, label)
	return okEnvelope(pluginapi.CommandLineExecutionResponse{
		Stdout: []byte(message),
		Auths:  []pluginapi.AuthData{authData},
	})
}

// flagProvided reports whether a flag was explicitly set with a value.
func flagProvided(flags map[string]pluginapi.CommandLineFlagValue, name string) bool {
	value, ok := flags[name]
	return ok && value.Set && strings.TrimSpace(value.Value) != ""
}

// flagTrue reports whether a boolean flag was set to true.
func flagTrue(flags map[string]pluginapi.CommandLineFlagValue, name string) bool {
	value, ok := flags[name]
	if !ok {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(value.Value), "true")
}

// readLine reads one line from standard input.
func readLine() (string, error) {
	reader := bufio.NewReader(os.Stdin)
	line, errRead := reader.ReadString('\n')
	if errRead != nil && strings.TrimSpace(line) == "" {
		return "", errRead
	}
	return line, nil
}

// openBrowser opens a URL with the platform browser launcher.
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}
