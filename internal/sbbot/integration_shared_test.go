package sbbot

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"
)

// Launched by the controller's isolated Unix-socket integration harness.
func TestBotSharedControllerIntegration(t *testing.T) {
	socket, path := os.Getenv("SBBOT_TEST_CONTROLLER_SOCKET"), os.Getenv("SBBOT_TEST_CONFIG")
	if socket == "" || path == "" {
		t.Skip("requires explicit isolated controller")
	}
	c := &ConfigManager{Path: path, ControlSocket: socket}
	ctx := context.Background()
	d, e := Load(path)
	if e != nil {
		t.Fatal(e)
	}
	if e = c.BridgeCall(ctx, "/integration-web-save", map[string]string{}, nil); e != nil {
		t.Fatal(e)
	}
	d["outbounds"] = append(list(d["outbounds"]), Doc{"tag": "bot-created", "type": "direct"})
	if e = c.Apply(ctx, d, nil); e != nil {
		t.Fatal(e)
	}
	found := false
	for _, v := range list(d["outbounds"]) {
		if str(obj(v)["tag"]) != "direct" && str(obj(v)["tag"]) != "relay" && str(obj(v)["tag"]) != "bot-created" {
			found = true
		}
	}
	if !found {
		t.Fatal("Bot failed to re-read merged Web edit")
	}
	var web struct{ Items []struct{ Tag string } }
	if e = c.BridgeCall(ctx, "/outbound-catalog", map[string]string{}, &web); e != nil {
		t.Fatal(e)
	}
	found = false
	for _, v := range web.Items {
		if v.Tag == "bot-created" {
			found = true
		}
	}
	if !found {
		t.Fatal("Web failed to read Bot edit")
	}
	stale, e := Load(path)
	if e != nil {
		t.Fatal(e)
	}
	if e = c.BridgeCall(ctx, "/integration-web-edit", map[string]string{}, nil); e != nil {
		t.Fatal(e)
	}
	before, _ := os.ReadFile(path)
	for _, v := range list(stale["outbounds"]) {
		o := obj(v)
		if str(o["tag"]) == "relay" {
			o["username"] = "bot"
		}
	}
	if e = c.Apply(ctx, stale, nil); e == nil {
		t.Fatal("same target silently overwritten")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("conflict mutated config")
	}
	raw, _ := json.Marshal(d["extension"])
	if !bytes.Contains(raw, []byte("9007199254740993")) {
		t.Fatal("extension precision lost")
	}
}
