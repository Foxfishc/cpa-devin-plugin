# cpa-devin-plugin

A CLIProxyAPI dynamic-library plugin that adds **Devin Desktop** (formerly
Windsurf/Codeium Cascade) as a first-class OAuth provider, model provider, and
executor.  The plugin communicates with Devin's native Connect-RPC / Protocol
Buffers endpoint and exposes it through CLIProxyAPI's standard OpenAI-compatible
API surface.

## Features

- **Browser one-time-token login** – opens `https://windsurf.com/show-auth-token`,
  the user copies the short-lived token and submits it through a plugin-owned
  Management API route.
- **Local Desktop import** – reads the session token from a locally installed
  Devin Desktop's `state.vscdb` SQLite database.
- **Token exchange & refresh** – exchanges the one-time token via
  `RegisterUser` / `MigrateApiKey` and refreshes via `GetSelfDevinSessionToken`.
- **Model discovery** – queries `GetCascadeModelConfigs` for the live model
  catalog; falls back to static models if discovery fails.
- **Chat executor** – translates OpenAI chat-completion requests into Devin
  protobuf `GetChatMessage` calls with both non-streaming and streaming support.
- **CLI capability** – exposes a `--devin-login` equivalent through the plugin
  command-line capability.
- **Management API** – exposes `POST /v0/management/devin/submit-token` for
  manual token submission during the browser login flow.

## Build

Requirements:

- Go 1.26+
- A C compiler (e.g. MinGW-GCC on Windows)
- The CLIProxyAPI source tree checked out as a sibling directory

```powershell
cd cpa-devin-plugin
$env:CGO_ENABLED = "1"
go build -buildmode=c-shared -o devin-desktop.dll .
```

The resulting `devin-desktop.dll` (plus the generated `.h` header) is the
loadable plugin.

## Install

1. Copy `devin-desktop.dll` into the CLIProxyAPI plugin directory:

   ```
   <CLIProxyAPI>/plugins/windows/amd64/devin-desktop.dll
   ```

2. Enable plugins in `config.yaml`:

   ```yaml
   plugins:
     enabled: true
     dir: "plugins"
     configs:
       devin-desktop:
         enabled: true
         priority: 1
   ```

3. (Optional) Configure a management secret so the Management API is available:

   ```yaml
   remote-management:
     secret-key: "<bcrypt hash of your management password>"
   ```

## Login

### Browser one-time-token flow

1. Start the login by visiting (with management auth):

   ```
   GET /v0/management/devin-auth-url
   ```

   The response contains `url` and `state`:

   ```json
   {"status":"ok","url":"https://windsurf.com/show-auth-token","state":"devin-..."}
   ```

2. Open the URL in a browser, sign in, and copy the displayed one-time token.

3. Submit the token:

   ```
   POST /v0/management/devin/submit-token
   {"state":"devin-...","token":"<one-time-token>"}
   ```

4. Poll until complete:

   ```
   GET /v0/management/get-auth-status?state=devin-...
   ```

   On success the auth record is persisted automatically.

### Local Desktop import

Set `import_from_desktop: true` in the plugin config and use the CLI login
command.  The plugin will search for the Devin Desktop `state.vscdb` file and
extract the `apiKey` from the `windsurfAuthStatus` row.

## Configuration

| Field                 | Type    | Default                       | Description                                              |
|-----------------------|---------|-------------------------------|----------------------------------------------------------|
| `enabled`             | bool    | `true`                        | When false, the plugin declines all requests.            |
| `base_url`            | string  | `https://server.codeium.com`  | Devin Connect RPC base URL.                              |
| `login_url`           | string  | `https://windsurf.com/show-auth-token` | Browser page that displays the auth token.       |
| `client_name`         | string  | `chisel`                      | Client identity reported to Devin.                       |
| `client_version`      | string  | `3000.2.17`                   | Client version reported to Devin.                        |
| `os`                  | enum    | host platform                 | `mac`, `win`, or `linux`.                                |
| `locale`              | string  | `en`                          | Locale reported to Devin.                                |
| `import_from_desktop` | bool    | `false`                       | Allow importing from a locally installed Devin Desktop.  |
| `desktop_state_db`    | string  | auto-detect                   | Explicit path to `state.vscdb`.                          |
| `max_tokens`          | int     | `128000`                      | Upstream completion `max_tokens`.                        |

## Architecture

```
main.go          C ABI entry point + JSON method dispatcher
config.go        Plugin configuration parsing
auth.go          AuthProvider: Parse, Refresh, StorageJSON
login.go         StartLogin / PollLogin state machine
devinclient.go   Devin Connect-RPC client (RegisterUser, Migrate, Refresh, Models, Chat)
hostcall.go      Host bridge helpers (HTTP, streaming, auth storage)
management.go    Management API route for token submission
models.go        ModelProvider: static + live model discovery
chat.go          OpenAI <-> Devin protobuf message translation
executor.go      Executor: non-streaming + streaming GetChatMessage
cli.go           CLI capability for login/import
```

The plugin uses generated protobuf bindings from the Devin/Windsurf protocol
definition.  All cross-ABI communication uses JSON envelopes and the host's
HTTP/streaming bridge — no Go interfaces, slices, or channels cross the ABI
boundary.

## Notes

- Devin's browser flow does **not** provide a localhost OAuth redirect.  The
  plugin uses a manual token submission approach via the Management API.
- The session token is never logged.  Auth files are stored through the host's
  standard auth storage with restrictive permissions.
- The plugin is a trusted in-process dynamic library loaded by CLIProxyAPI.
