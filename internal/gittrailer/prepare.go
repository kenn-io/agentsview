package gittrailer

import (
	"bytes"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"os"
)

const (
	prepareHeader   = "agentsview-prepare-v1\n"
	prepareSuffix   = ".agentsview-prepare"
	maxPrepareBytes = 1024
)

type prepareState struct {
	Source       string `json:"source"`
	Invocation   string `json:"invocation"`
	Ready        bool   `json:"ready"`
	TemplateHash string `json:"template_hash,omitempty"`
}

func prepareSource(source string) bool {
	switch source {
	case "message", "template", "editor", "commit", "merge", "squash":
		return true
	}
	return false
}

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func readPrepare(path string) (prepareState, bool, error) {
	data, _, err := readRegular(path+prepareSuffix, maxPrepareBytes)
	if errors.Is(err, os.ErrNotExist) {
		return prepareState{}, false, nil
	}
	if err != nil {
		return prepareState{}, false, errors.New("cannot inspect owned prepare context")
	}
	var state prepareState
	body, recognized := bytes.CutPrefix(data, []byte(prepareHeader))
	if !recognized || json.Unmarshal(body, &state, json.RejectUnknownMembers(true)) != nil ||
		!prepareSource(state.Source) ||
		state.Invocation != "" && !validDigest(state.Invocation) ||
		(!state.Ready || state.Source != "template") && state.TemplateHash != "" ||
		state.Ready && state.Source == "template" && !validDigest(state.TemplateHash) {
		return prepareState{}, false, errors.New("unrecognized prepare context; leaving it untouched")
	}
	return state, true, nil
}

func writePrepare(path string, state prepareState) error {
	body, err := json.Marshal(state)
	if err != nil {
		return errors.New("cannot encode prepare context")
	}
	data := append([]byte(prepareHeader), body...)
	if len(data) > maxPrepareBytes {
		return errors.New("prepare context exceeds byte limit")
	}
	return atomicWrite(path+prepareSuffix, data, 0o600)
}
