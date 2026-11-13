package argo

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hua-ri/coxswain/internal/action"
	"github.com/hua-ri/coxswain/internal/argo"
)

type loginInput struct {
	Instance string      `json:"instance"`
	Method   argo.Method `json:"method"`
	Username string      `json:"username"`
	Password string      `json:"password"`
	Token    string      `json:"token"`
}

func (p *Provider) loginMethods(k argo.Kind) *action.Action {
	schema := map[string]any{"type": "object", "properties": map[string]any{"instance": instanceProperty}}
	return &action.Action{
		ID:      string(k) + ".login_methods",
		Summary: "List the ways to log in to a " + kindName(k) + " instance",
		Description: "Asks the server which login methods it offers and returns them with the current login state. " +
			"The user chooses the method; " + string(k) + "_login performs it.",
		Input:       schema,
		Positional:  []string{"instance"},
		Annotations: action.Annotations{ReadOnly: true, Idempotent: true, OpenWorld: true},
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			var in loginInput
			if err := decode(raw, schema, &in); err != nil {
				return action.Assessment{}, err
			}
			i, err := p.instance(k, in.Instance)
			if err != nil {
				return action.Assessment{}, err
			}
			return action.Assessment{Risk: action.Read, Targets: []action.Target{target(i, "")}, Preview: "login methods of " + i.Name}, nil
		},
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in loginInput
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			i, err := p.instance(k, in.Instance)
			if err != nil {
				return nil, err
			}
			if k == argo.CD {
				return p.Sessions.CDMethods(ctx, i), nil
			}
			return p.Sessions.WFMethods(ctx, i), nil
		},
	}
}

func (p *Provider) login(k argo.Kind) *action.Action {
	props := map[string]any{
		"instance": instanceProperty,
		"token": map[string]any{"type": "string", "writeOnly": true,
			"description": "for method token; kept in the keychain and never shown again (a leading \"Bearer \" is fine)"},
	}
	methods := []string{"token", "none"}
	desc := "Logs in to an Argo Workflows instance. argo-server has no accounts: method token takes a Kubernetes token " +
		"(client auth mode) or the token from the Argo UI's user info page (sso mode); method none is for server auth mode. " +
		"The credential is checked, then kept in the macOS keychain; no action ever returns it. " +
		"Prefer letting the user enter a token in Coxswain over passing one through your context."
	if k == argo.CD {
		methods = []string{"browser", "password", "token"}
		props["username"] = str("for method password: an Argo CD local account, e.g. admin")
		props["password"] = map[string]any{"type": "string", "writeOnly": true,
			"description": "for method password; exchanged for a session token, never stored or shown"}
		desc = "Logs in to an Argo CD instance as argocd login does. method browser opens the system browser on the user's Mac " +
			"for the instance's single sign-on and waits (up to 5 minutes) for the user to finish; tell the user to complete it. " +
			"method password exchanges a local account's password for a session token; method token keeps an API token. " +
			"Tokens are kept in the macOS keychain; no action ever returns them. " +
			"Prefer browser, or letting the user log in in Coxswain, over passing a password through your context."
	}
	props["method"] = map[string]any{"type": "string", "enum": methods, "description": "see " + string(k) + "_login_methods"}
	schema := map[string]any{"type": "object", "properties": props, "required": []string{"method"}}
	return &action.Action{
		ID:          string(k) + ".login",
		Summary:     "Log in to a " + kindName(k) + " instance",
		Description: desc,
		Input:       schema,
		Positional:  []string{"instance"},
		Annotations: action.Annotations{OpenWorld: true},
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			var in loginInput
			if err := decode(raw, schema, &in); err != nil {
				return action.Assessment{}, err
			}
			i, err := p.instance(k, in.Instance)
			if err != nil {
				return action.Assessment{}, err
			}
			preview := fmt.Sprintf("log in to %s with %s", i.Name, in.Method)
			if in.Username != "" {
				preview += " as " + in.Username
			}
			return action.Assessment{Risk: action.Write, Targets: []action.Target{target(i, "")}, Preview: preview}, nil
		},
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in loginInput
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			i, err := p.instance(k, in.Instance)
			if err != nil {
				return nil, err
			}
			r := argo.LoginRequest{Method: in.Method, Username: in.Username, Password: in.Password, Token: in.Token}
			var st argo.Status
			if k == argo.CD {
				st, err = p.Sessions.CDLogin(ctx, i, r)
			} else {
				st, err = p.Sessions.WFLogin(ctx, i, r)
			}
			if err != nil {
				return nil, err
			}
			return InstanceView{Instance: i, Login: st}, nil
		},
	}
}

func (p *Provider) logout(k argo.Kind) *action.Action {
	schema := map[string]any{"type": "object", "properties": map[string]any{"instance": instanceProperty}}
	return &action.Action{
		ID:          string(k) + ".logout",
		Summary:     "Forget the login of a " + kindName(k) + " instance",
		Description: "Deletes the instance's credential from the keychain (as argocd logout, the server is not told).",
		Input:       schema,
		Positional:  []string{"instance"},
		Annotations: action.Annotations{Idempotent: true},
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			var in loginInput
			if err := decode(raw, schema, &in); err != nil {
				return action.Assessment{}, err
			}
			i, err := p.instance(k, in.Instance)
			if err != nil {
				return action.Assessment{}, err
			}
			return action.Assessment{Risk: action.Write, Targets: []action.Target{target(i, "")}, Preview: "log out of " + i.Name}, nil
		},
		Run: func(_ context.Context, raw json.RawMessage) (any, error) {
			var in loginInput
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			i, err := p.instance(k, in.Instance)
			if err != nil {
				return nil, err
			}
			p.Sessions.CancelLogin(i)
			return p.view(i), p.Sessions.Logout(i)
		},
	}
}

func (p *Provider) cancelLogin(k argo.Kind) *action.Action {
	schema := map[string]any{"type": "object", "properties": map[string]any{"instance": instanceProperty}}
	return &action.Action{
		ID:          string(k) + ".cancel_login",
		Summary:     "Stop waiting for a browser login",
		Description: "Cancels a browser login that is waiting for the user to finish in the browser.",
		Input:       schema,
		Positional:  []string{"instance"},
		Annotations: action.Annotations{Idempotent: true},
		Label: func(_ context.Context, raw json.RawMessage) (action.Assessment, error) {
			var in loginInput
			if err := decode(raw, schema, &in); err != nil {
				return action.Assessment{}, err
			}
			i, err := p.instance(k, in.Instance)
			if err != nil {
				return action.Assessment{}, err
			}
			return action.Assessment{Risk: action.Read, Targets: []action.Target{target(i, "")}, Preview: "cancel login to " + i.Name}, nil
		},
		Run: func(_ context.Context, raw json.RawMessage) (any, error) {
			var in loginInput
			if err := action.Decode(raw, &in); err != nil {
				return nil, err
			}
			i, err := p.instance(k, in.Instance)
			if err != nil {
				return nil, err
			}
			return map[string]bool{"cancelled": p.Sessions.CancelLogin(i)}, nil
		},
	}
}
