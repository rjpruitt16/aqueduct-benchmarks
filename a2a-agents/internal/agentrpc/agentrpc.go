// Package agentrpc is the A2A plumbing the agents share: serving an agent
// over JSON-RPC with its Agent Card, calling another agent, and moving JSON
// values in data parts.
package agentrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// Handler turns one request's data into the agent's reply data.
type Handler func(ctx context.Context, in json.RawMessage) (any, error)

// Mux returns an HTTP handler serving one agent: JSON-RPC at "/", its Agent
// Card at the well-known path. publicURL is the address other agents use.
func Mux(name, description, publicURL string, h Handler) http.Handler {
	exec := a2asrv.AgentExecutorFunc(func(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			in, err := DataOf(ec.Message)
			if err != nil {
				yield(nil, fmt.Errorf("%w: %v", a2a.ErrInvalidParams, err))
				return
			}
			out, err := h(ctx, in)
			if err != nil {
				yield(nil, err)
				return
			}
			yield(a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewDataPart(Plain(out))), nil)
		}
	})
	card := &a2a.AgentCard{
		Name: name, Description: description,
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(publicURL, a2a.TransportProtocolJSONRPC)},
		DefaultInputModes:   []string{"application/json"}, DefaultOutputModes: []string{"application/json"},
		Skills: []a2a.AgentSkill{{ID: name, Name: name, Description: description}},
	}
	mux := http.NewServeMux()
	mux.Handle("/", a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(exec)))
	mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))
	return mux
}

// Plain turns v into plain JSON values (maps, slices, strings, numbers). The
// SDK's task store copies artifacts with gob, which only knows plain values,
// so a struct must go into a data part in this form.
func Plain(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out any
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

// DataOf returns the first data part of msg as JSON.
func DataOf(msg *a2a.Message) (json.RawMessage, error) {
	if msg == nil {
		return nil, errors.New("no message")
	}
	return dataOfParts(msg.Parts)
}

func dataOfParts(parts []*a2a.Part) (json.RawMessage, error) {
	for _, p := range parts {
		if d := p.Data(); d != nil {
			return json.Marshal(d)
		}
	}
	return nil, errors.New("no data part")
}

var (
	clientsMu sync.Mutex
	clients   = map[string]*a2aclient.Client{}
)

// Client returns a JSON-RPC A2A client for the agent at url, reused across
// calls. Its HTTP client has no timeout: the SDK's default (3 minutes) would
// cut off workflows that are still running; callers bound calls with ctx.
func Client(ctx context.Context, url string) (*a2aclient.Client, error) {
	clientsMu.Lock()
	defer clientsMu.Unlock()
	if c, ok := clients[url]; ok {
		return c, nil
	}
	c, err := a2aclient.NewFromEndpoints(ctx, []*a2a.AgentInterface{a2a.NewAgentInterface(url, a2a.TransportProtocolJSONRPC)},
		a2aclient.WithJSONRPCTransport(&http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 256}}))
	if err != nil {
		return nil, err
	}
	clients[url] = c
	return c, nil
}

// Call sends in to the agent at url and decodes its reply data into out.
func Call(ctx context.Context, url string, in, out any) error {
	c, err := Client(ctx, url)
	if err != nil {
		return err
	}
	res, err := c.SendMessage(ctx, &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewDataPart(Plain(in)))})
	if err != nil {
		return err
	}
	var raw json.RawMessage
	switch r := res.(type) {
	case *a2a.Message:
		raw, err = DataOf(r)
	case *a2a.Task:
		if r.Status.State != a2a.TaskStateCompleted {
			return fmt.Errorf("agent task ended %s", r.Status.State)
		}
		for _, art := range r.Artifacts {
			if raw, err = dataOfParts(art.Parts); err == nil {
				break
			}
		}
	default:
		return fmt.Errorf("unexpected reply %T", res)
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}
