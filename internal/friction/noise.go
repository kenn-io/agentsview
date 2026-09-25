package friction

import (
	"regexp"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/serdejson"
)

// Expected-noise allowlist, ported verbatim from jilog detectors.rs:279-488
// (jilog#42fd). Only positively identified shapes are suppressed; anything
// unrecognized is emitted.

// bareTimeoutRe is detectors.rs:326. RE2 \d is ASCII-only (Rust's is
// Unicode); pinned by TestBareTimeoutDigitClass.
var bareTimeoutRe = regexp.MustCompile(`(?i)^command timed out after \d+ seconds?\.?$`)

var (
	bareTopKeys    = []string{"success", "error", "output", "message"}
	bareOutputKeys = []string{"returncode", "stdout", "stderr"}
	bareErrorKeys  = []string{"message"}
)

func isExpectedNoise(toolName string, data map[string]any) bool {
	return isModeDenial(toolName, data) || isContentFreeBashFailure(toolName, data)
}

// isModeDenial is detectors.rs:299-320.
func isModeDenial(toolName string, data map[string]any) bool {
	if toolName != "mode" {
		return false
	}
	output, _ := data["output"].(map[string]any)
	statusDenied := output["status"] == "denied"
	_, hasDeniedMode := output["denied_mode"]
	errObj, _ := data["error"].(map[string]any)
	codeDenied := false
	for _, c := range []any{errObj["code"], data["code"]} {
		if s, ok := c.(string); ok && strings.HasSuffix(s, "_denied") {
			codeDenied = true
		}
	}
	return statusDenied || hasDeniedMode || codeDenied
}

type errTextKind int

const (
	errTextAbsent errTextKind = iota
	errTextText
	errTextUnrecognized
)

// errorText is detectors.rs:347-368.
func errorText(data map[string]any) (errTextKind, string) {
	hasMessage := data["message"] != nil
	errV := data["error"]
	if errV == nil {
		switch m := data["message"].(type) {
		case string:
			return errTextText, m
		case nil:
			return errTextAbsent, ""
		default:
			return errTextUnrecognized, ""
		}
	}
	if hasMessage {
		return errTextUnrecognized, ""
	}
	switch e := errV.(type) {
	case string:
		return errTextText, e
	case map[string]any:
		if s, ok := e["message"].(string); ok {
			return errTextText, s
		}
		return errTextUnrecognized, ""
	default:
		return errTextUnrecognized, ""
	}
}

func keysWithin(m map[string]any, allowed []string) bool {
	for k := range m {
		if !slices.Contains(allowed, k) {
			return false
		}
	}
	return true
}

// envelopeIsBare is detectors.rs:405-421.
func envelopeIsBare(data map[string]any) bool {
	if !keysWithin(data, bareTopKeys) {
		return false
	}
	errorOK := false
	switch e := data["error"].(type) {
	case nil, string:
		errorOK = true
	case map[string]any:
		errorOK = keysWithin(e, bareErrorKeys)
	}
	outputOK := false
	switch o := data["output"].(type) {
	case nil, string:
		outputOK = true
	case map[string]any:
		outputOK = keysWithin(o, bareOutputKeys)
	}
	return errorOK && outputOK
}

// outputText is detectors.rs:373-380.
func outputText(output map[string]any, key string) (string, bool) {
	switch v := output[key].(type) {
	case nil:
		return "", true
	case string:
		return v, true
	default:
		return "", false
	}
}

// isInt64 mirrors serde_json Value::as_i64().is_some().
func isInt64(v any) (int64, bool) {
	n, ok := v.(serdejson.Number)
	if !ok {
		return 0, false
	}
	return n.Int64()
}

// outputIsBlank is detectors.rs:432-456.
func outputIsBlank(data map[string]any) bool {
	switch o := data["output"].(type) {
	case nil:
		return true
	case string:
		t := strings.TrimSpace(o)
		return t == "" || bareTimeoutRe.MatchString(t)
	case map[string]any:
		returncodeOK := true
		if rc, present := o["returncode"]; present {
			_, returncodeOK = isInt64(rc)
		}
		stdout, okOut := outputText(o, "stdout")
		stderr, okErr := outputText(o, "stderr")
		return returncodeOK && okOut && okErr &&
			strings.TrimSpace(stdout) == "" && strings.TrimSpace(stderr) == ""
	default:
		return false
	}
}

// isBareTimeout is detectors.rs:460-466.
func isBareTimeout(data map[string]any) bool {
	kind, text := errorText(data)
	return envelopeIsBare(data) && kind == errTextText &&
		bareTimeoutRe.MatchString(strings.TrimSpace(text)) && outputIsBlank(data)
}

// isBareNonzeroExit is detectors.rs:472-483.
func isBareNonzeroExit(data map[string]any) bool {
	output, _ := data["output"].(map[string]any)
	rc, ok := isInt64(output["returncode"])
	nonzero := ok && rc != 0
	kind, _ := errorText(data)
	return envelopeIsBare(data) && nonzero && kind == errTextAbsent && outputIsBlank(data)
}

// isContentFreeBashFailure is detectors.rs:486-488.
func isContentFreeBashFailure(toolName string, data map[string]any) bool {
	return toolName == "bash" && (isBareTimeout(data) || isBareNonzeroExit(data))
}
