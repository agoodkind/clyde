package config

import (
	"fmt"
	"strings"
)

// applyMITMListenerDefaultsAndValidate flattens the [mitm.cli.<name>] and
// [mitm.app.<name>] groups into the canonical id->endpoint Listeners map. Each
// id is group-qualified ("cli.<name>" / "app.<name>") so the same leaf name (for
// example "codex") can appear in both groups without colliding on the SQLite
// capture `client` tag or the reload-inherited file descriptor key. When both
// groups are empty it synthesizes a single "default" listener. It lives in its
// own file so load.go stays under the file-length gate.
func applyMITMListenerDefaultsAndValidate(mitm *MITMConfig) error {
	listeners := make(map[string]MITMListenerConfig, len(mitm.App)+len(mitm.CLI))
	if err := flattenMITMListenerGroup("cli", mitm.CLI, listeners); err != nil {
		return err
	}
	if err := flattenMITMListenerGroup("app", mitm.App, listeners); err != nil {
		return err
	}
	if len(listeners) == 0 {
		listeners[defaultMITMListenerID] = MITMListenerConfig{
			ID:   defaultMITMListenerID,
			Host: defaultMITMListenHost,
			Port: defaultMITMListenPort,
		}
	}
	mitm.Listeners = listeners
	return nil
}

// flattenMITMListenerGroup normalizes every member of one [mitm.<group>.<name>]
// table and writes it into out under the group-qualified id "<group>.<name>".
func flattenMITMListenerGroup(group string, members map[string]MITMListenerConfig, out map[string]MITMListenerConfig) error {
	for name, listener := range members {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("mitm.%s has a listener with an empty name; listener names are required", group)
		}
		id := group + "." + name
		if err := normalizeMITMListener(id, &listener); err != nil {
			return err
		}
		out[id] = listener
	}
	return nil
}
