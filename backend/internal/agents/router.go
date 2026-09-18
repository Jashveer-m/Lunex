package agents

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jashveer/lifeos/backend/internal/ai"
	"github.com/jashveer/lifeos/backend/internal/tools"
)

// Catalog is what the router needs from the tool registry: the declarations of
// the tools, to render into the prompt and to check a decision against. It
// cannot run anything.
type Catalog interface {
	Tools() []tools.Tool
}

// Options are the routing knobs, resolved from configuration at boot.
type Options struct {
	// Model overrides the provider's own model for routing.
	Model string
	// Temperature is near zero: this is a classification, and the same message
	// should get the same decision.
	Temperature float64
	// MaxTokens caps the reply. A decision is one short JSON object.
	MaxTokens int
	// Timeout bounds one decision. It is spent before the first token of the
	// answer, so it is the latency tools add to every turn.
	Timeout time.Duration
}

// Routing defaults, applied to a zero Options.
const (
	DefaultTemperature = 0.1
	DefaultMaxTokens   = 200
	DefaultTimeout     = 180 * time.Second
)

// Router makes the per-message decision.
//
// It is the structured-output pattern of Phase 5 and 6's extractors, not the
// provider's native tool calling, and that choice was measured rather than
// assumed: see the package comment on ParseDecision and docs/decisions.md. In
// short, llama3.2:3b with Ollama's native tools called a tool on every one of
// eight messages that needed none -- "Thanks" produced a create_task with a
// deadline in 2024 -- and this prompt called none on all eight while choosing
// the right tool for the other ten.
type Router struct {
	provider ai.Provider
	catalog  Catalog
	log      *slog.Logger
	opts     Options
}

func NewRouter(provider ai.Provider, catalog Catalog, log *slog.Logger, opts Options) *Router {
	if opts.Temperature == 0 {
		opts.Temperature = DefaultTemperature
	}
	if opts.MaxTokens <= 0 {
		opts.MaxTokens = DefaultMaxTokens
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if log == nil {
		log = slog.Default()
	}
	return &Router{provider: provider, catalog: catalog, log: log, opts: opts}
}

// Decide asks the model whether the message needs one of the agent's tools.
//
// A reply that names no tool, an unknown tool, or a tool this agent may not
// use is "no tool" -- the model's judgement, logged, and not an error. What
// is an error is the model not answering at all: the call failed, or it ran
// past Options.Timeout. Both are reported as ai.ErrUnavailable, so the turn
// answers 503 before its stream starts -- the same as any other model outage
// -- rather than answering without the step that decides whether the user
// asked for something to be done.
func (r *Router) Decide(ctx context.Context, agent Agent, message string) (Decision, error) {
	offered := r.offered(agent)
	if len(offered) == 0 {
		return Decision{}, nil
	}

	callCtx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()

	stream, err := r.provider.Chat(callCtx, RoutingPrompt(offered, message), ai.Options{
		Model: r.opts.Model, Temperature: r.opts.Temperature, MaxTokens: r.opts.MaxTokens,
	})
	if err != nil {
		return Decision{}, r.failure(ctx, err)
	}
	defer stream.Close() //nolint:errcheck // releases the upstream connection
	reply, err := ai.Collect(stream)
	if err != nil {
		return Decision{}, r.failure(ctx, err)
	}

	d := ParseDecision(reply)
	switch {
	case d.None():
		return Decision{}, nil
	case !agent.Allows(d.Tool) || !offeredHas(offered, d.Tool):
		r.log.Info("routing chose a tool it was not offered; using none",
			"agent", agent.Name, "tool", d.Tool)
		return Decision{}, nil
	}
	r.dropUngroundedArguments(offered, &d, message)
	return d, nil
}

// dropUngroundedArguments removes every argument whose value the message does
// not give and whose parameter says it must, so the tool runs as if the model
// had left it out -- which is what it should have done. See
// tools.ValueGrounded for the measured failures.
//
// Dropped rather than refused: the rest of the call is usually right ("search
// my notes for seedlings" with an invented tag is still a search for
// seedlings), and running it without the argument answers the question the
// user asked -- or, for a write, proposes what the user actually said with the
// invented detail left at its default, which the proposal then shows them.
//
// Most of what is checked is read filters, because a made-up filter is the
// argument a user cannot tell from the truth. The exception is a write
// argument that behaves like one -- see tools.Param.Grounded. What is *not*
// checked is the rest of a write's arguments: a title is the model's wording
// of what the user asked for, and the user reads it on the proposal.
func (r *Router) dropUngroundedArguments(offered []tools.Tool, d *Decision, message string) {
	for _, t := range offered {
		if t.Name != d.Tool {
			continue
		}
		for _, p := range t.Params {
			if !p.MustBeGrounded() {
				continue
			}
			// Every key the tool reads this argument from, not only its
			// declared name: a value written under an alias the check skipped
			// would reach the tool exactly as an invented one does.
			for _, key := range p.Keys() {
				raw, present := d.Args[key]
				if !present {
					continue
				}
				if value := d.Args.String(key); value != "" && tools.ValueGrounded(p, value, message) {
					continue
				}
				delete(d.Args, key)
				r.log.Info("routing dropped an argument the message does not give",
					"tool", d.Tool, "param", p.Name, "key", key, "value", raw)
			}
		}
	}
}

// offered is the agent's tools that the registry actually has, in the
// registry's order -- which puts the read tools first, as the prompt was
// measured with.
func (r *Router) offered(agent Agent) []tools.Tool {
	var out []tools.Tool
	for _, t := range r.catalog.Tools() {
		if agent.Allows(t.Name) {
			out = append(out, t)
		}
	}
	return out
}

func offeredHas(offered []tools.Tool, name string) bool {
	for _, t := range offered {
		if t.Name == name {
			return true
		}
	}
	return false
}

// failure classifies a routing call that produced no decision. The caller's
// own cancellation is passed through untouched -- a client that hung up is
// not an outage -- and everything else, the router's own deadline included,
// is the model being unavailable.
func (r *Router) failure(parent context.Context, err error) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: the model did not choose a tool within %s", ai.ErrUnavailable, r.opts.Timeout)
	}
	if errors.Is(err, ai.ErrUnavailable) {
		return fmt.Errorf("route: %w", err)
	}
	return fmt.Errorf("%w: route: %v", ai.ErrUnavailable, err)
}
