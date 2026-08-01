// Package codex adapts Codex App Server to llmprovider's chat interface.
package codex

import (
	"encoding/json"
	"io"
	"os"
)

type SandboxMode string

const (
	SandboxReadOnly         SandboxMode = "read-only"
	SandboxWorkspaceWrite   SandboxMode = "workspace-write"
	SandboxDangerFullAccess SandboxMode = "danger-full-access"
)

type ApprovalPolicy string

const (
	ApprovalUntrusted ApprovalPolicy = "untrusted"
	ApprovalOnRequest ApprovalPolicy = "on-request"
	ApprovalNever     ApprovalPolicy = "never"
)

// RequestHandler handles a server-initiated JSON-RPC request. Returning an
// error sends a JSON-RPC error response back to App Server.
type RequestHandler func(method string, params json.RawMessage) (any, error)

type config struct {
	command                 string
	args                    []string
	environment             []string
	stderr                  io.Writer
	clientName              string
	clientTitle             string
	clientVersion           string
	model                   string
	baseInstructions        string
	cwd                     string
	sandbox                 SandboxMode
	approvalPolicy          ApprovalPolicy
	ephemeral               bool
	serviceName             string
	requestHandler          RequestHandler
	experimentalAPI         bool
	transportFactoryForTest func() (transport, error)
}

type Option func(*config)

// WithCommand replaces the default "codex app-server --listen stdio://"
// command. It is useful for wrappers or a non-default Codex executable.
func WithCommand(command string, args ...string) Option {
	return func(c *config) {
		c.command = command
		c.args = append([]string(nil), args...)
	}
}

func WithEnvironment(values ...string) Option {
	return func(c *config) { c.environment = append(c.environment, values...) }
}

func WithStderr(writer io.Writer) Option {
	return func(c *config) { c.stderr = writer }
}

func WithClientInfo(name, title, version string) Option {
	return func(c *config) {
		c.clientName, c.clientTitle, c.clientVersion = name, title, version
	}
}

func WithModel(model string) Option {
	return func(c *config) { c.model = model }
}

// WithBaseInstructions replaces Codex's built-in base instructions for new
// threads. System and developer chat messages remain additive through
// developerInstructions.
func WithBaseInstructions(instructions string) Option {
	return func(c *config) { c.baseInstructions = instructions }
}

func WithWorkingDirectory(cwd string) Option {
	return func(c *config) { c.cwd = cwd }
}

func WithSandbox(mode SandboxMode) Option {
	return func(c *config) { c.sandbox = mode }
}

func WithApprovalPolicy(policy ApprovalPolicy) Option {
	return func(c *config) { c.approvalPolicy = policy }
}

// WithEphemeral controls whether new Codex threads are written to disk. It is
// true by default, matching the stateless feel of chat/completions.
func WithEphemeral(ephemeral bool) Option {
	return func(c *config) { c.ephemeral = ephemeral }
}

func WithServiceName(name string) Option {
	return func(c *config) { c.serviceName = name }
}

func WithRequestHandler(handler RequestHandler) Option {
	return func(c *config) { c.requestHandler = handler }
}

// WithExperimentalAPI controls App Server's experimental API capability.
// Dynamic tool calling requires it and it is enabled by default.
func WithExperimentalAPI(enabled bool) Option {
	return func(c *config) { c.experimentalAPI = enabled }
}

func defaultConfig() config {
	return config{
		command:         "codex",
		args:            []string{"app-server", "--listen", "stdio://"},
		environment:     os.Environ(),
		clientName:      "llm_provider",
		clientTitle:     "llm-provider",
		clientVersion:   "0.1.0",
		sandbox:         SandboxReadOnly,
		approvalPolicy:  ApprovalNever,
		ephemeral:       true,
		serviceName:     "llm-provider",
		experimentalAPI: true,
	}
}
