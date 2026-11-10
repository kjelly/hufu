package browser

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/kjelly/hufu/internal/waitbudget"
)

// Button identifies a visible accessibility button in one captured document.
// References are opaque, snapshot-scoped and never selectors or JavaScript.
type Button struct {
	Ref      string `json:"ref"`
	Name     string `json:"name"`
	Disabled bool   `json:"disabled"`
}

type buttonTarget struct {
	frame frameIdentity
	node  int
	name  string
}

type accessibilityNode struct {
	Ignored       bool `json:"ignored"`
	BackendNodeID int  `json:"backendDOMNodeId"`
	Role          struct {
		Value string `json:"value"`
	} `json:"role"`
	Name struct {
		Value string `json:"value"`
	} `json:"name"`
	Properties []struct {
		Name  string `json:"name"`
		Value struct {
			Value any `json:"value"`
		} `json:"value"`
	} `json:"properties"`
}

func readButtons(ctx context.Context, c *connection, frame frameIdentity) ([]Button, map[string]buttonTarget, error) {
	var tree struct {
		Nodes []accessibilityNode `json:"nodes"`
	}
	if err := c.call(ctx, "Accessibility.getFullAXTree", map[string]any{"frameId": frame.ID}, &tree); err != nil {
		return nil, nil, err
	}
	prefix := uuid.New().String()
	var buttons []Button
	targets := make(map[string]buttonTarget)
	for _, node := range tree.Nodes {
		if node.Ignored || node.Role.Value != "button" || node.BackendNodeID == 0 || len(buttons) == 128 {
			continue
		}
		disabled := false
		for _, property := range node.Properties {
			if property.Name == "disabled" && property.Value.Value == true {
				disabled = true
			}
		}
		ref := fmt.Sprintf("%s:%d", prefix, len(buttons)+1)
		name := node.Name.Value
		label := []rune(name)
		buttons = append(buttons, Button{Ref: ref, Name: string(label[:min(len(label), 512)]), Disabled: disabled})
		if !disabled {
			targets[ref] = buttonTarget{frame: frame, node: node.BackendNodeID, name: name}
		}
	}
	return buttons, targets, nil
}

// Click dispatches one actual pointer click to a current, unobscured button.
// It never accepts arbitrary coordinates, scripts, selectors, links or forms.
// Consumed references cannot be replayed, including after an uncertain failure.
func (b *CDP) Click(ctx context.Context, ref string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	target, ok := b.buttons[ref]
	if !ok {
		return errors.New("unknown, disabled or stale button reference; take a fresh snapshot")
	}
	b.buttons = nil
	ctx, cancel := waitbudget.WithWallDeadline(ctx, time.Now().Add(10*time.Second))
	defer cancel()
	c, err := b.page(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.ws.CloseNow() }()
	frame, err := c.frame(ctx)
	if err != nil {
		return err
	}
	if frame != target.frame {
		return errors.New("chrome document changed; take a fresh snapshot before clicking")
	}
	_, current, err := readButtons(ctx, c, frame)
	if err != nil {
		return err
	}
	matched := false
	for _, button := range current {
		if button.node == target.node && button.name == target.name {
			matched = true
			break
		}
	}
	if !matched {
		return errors.New("button changed or is no longer enabled; take a fresh snapshot")
	}
	var world struct {
		ID int `json:"executionContextId"`
	}
	if err := c.call(ctx, "Page.createIsolatedWorld", map[string]any{"frameId": frame.ID, "worldName": "hufu-observation"}, &world); err != nil {
		return err
	}
	if world.ID == 0 {
		return errors.New("chrome created no observation context")
	}
	var resolved struct {
		Object struct {
			ID string `json:"objectId"`
		} `json:"object"`
	}
	if err := c.call(ctx, "DOM.resolveNode", map[string]any{"backendNodeId": target.node, "executionContextId": world.ID}, &resolved); err != nil {
		return err
	}
	if resolved.Object.ID == "" {
		return errors.New("button no longer resolves to a DOM node")
	}
	if err := c.call(ctx, "DOM.scrollIntoViewIfNeeded", map[string]any{"backendNodeId": target.node}, nil); err != nil {
		return err
	}
	var position struct {
		Result struct {
			Value struct {
				X     float64 `json:"x"`
				Y     float64 `json:"y"`
				Valid bool    `json:"valid"`
			} `json:"value"`
		} `json:"result"`
		Exception jsontext.Value `json:"exceptionDetails"`
	}
	const locate = `function() { if (!this.isConnected || this.closest('form') || this.form || this.disabled || this.getAttribute('aria-disabled') === 'true' || this.closest('a[href]') || !(this.matches('button,input[type="button"],input[type="reset"],[role="button"]'))) throw new Error('button is not an allowed page control'); const s = getComputedStyle(this); const r = this.getBoundingClientRect(); if (s.visibility !== 'visible' || s.display === 'none' || Number(s.opacity) === 0 || r.width <= 0 || r.height <= 0) throw new Error('button is not visible'); const x = Math.max(0,r.left) + (Math.min(innerWidth,r.right)-Math.max(0,r.left))/2; const y = Math.max(0,r.top) + (Math.min(innerHeight,r.bottom)-Math.max(0,r.top))/2; const hit = document.elementFromPoint(x,y); if (x < 0 || y < 0 || x >= innerWidth || y >= innerHeight || !hit || !(hit === this || this.contains(hit))) throw new Error('button is obscured'); return {x,y,valid:true}; }`
	if err := c.call(ctx, "Runtime.callFunctionOn", map[string]any{"objectId": resolved.Object.ID, "functionDeclaration": locate, "returnByValue": true}, &position); err != nil {
		return err
	}
	if len(position.Exception) > 0 || !position.Result.Value.Valid {
		return errors.New("button is hidden, obscured, in a form or not an allowed page control; inspect a fresh snapshot or ask the operator")
	}
	frame, err = c.frame(ctx)
	if err != nil {
		return err
	}
	if frame != target.frame {
		return errors.New("chrome document changed before click; take a fresh snapshot")
	}
	for _, kind := range []string{"mousePressed", "mouseReleased"} {
		if err := c.call(ctx, "Input.dispatchMouseEvent", map[string]any{"type": kind, "x": position.Result.Value.X, "y": position.Result.Value.Y, "button": "left", "clickCount": 1}, nil); err != nil {
			return fmt.Errorf("click may have changed the page; inspect before retrying: %w", err)
		}
	}
	return nil
}
