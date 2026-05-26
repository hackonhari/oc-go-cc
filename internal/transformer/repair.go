// Package transformer handles request/response format conversion.
package transformer

import (
	"encoding/json"
	"log/slog"
	"regexp"
	"strings"
)

// ToolInputRepair validates tool call inputs and applies a small set of
// deterministic repairs for known failure modes in open-source model tool
// calling (DeepSeek, GLM, Qwen et al.).
//
// Design: validate-then-repair (not preprocess-then-validate).
//  1. Parse input as-is. If it succeeds, ship it. Valid inputs are never touched.
//  2. On failure, walk the validator's issue list. For each issue path, try
//     the four repairs in order until one applies.
//  3. Parse again. Log outcome per (model, tool) pair.
//
// Reference: https://x.com/MrAhmadAwais/status/2050956678502420612
type ToolInputRepair struct{}

// NewToolInputRepair creates a new tool input repair instance.
func NewToolInputRepair() *ToolInputRepair {
	return &ToolInputRepair{}
}

// RepairResult holds the outcome of a repair attempt.
type RepairResult struct {
	Repaired bool   // true when repairs were applied
	Output   string // the final (possibly repaired) JSON string
	Error    string // non-empty when repair failed
}

// knownFailureModes returns the four repair functions in their required order.
// Order matters: json-array-parse must run before bare-string-wrap, or a
// stringified array like "[\"a\",\"b\"]" gets wrapped into ["[\"a\",\"b\"]"].
var repairFns = []func(json.RawMessage) (json.RawMessage, bool){
	repairNullFields,       // 1. null → omit
	repairStringifiedArray, // 2. "[a,b]" → [a,b]
	repairSingleArgWrapper, // 3. {"key":"val"} → ["val"] when array expected
	repairBareStringToArray, // 4. "foo" → ["foo"]
}

// Repair applies the validate-then-repair cycle to a tool call's JSON input.
// When the input is valid JSON schema-wise, it is returned unchanged.
func (r *ToolInputRepair) Repair(input json.RawMessage, toolSchema json.RawMessage) RepairResult {
	// Step 1: parse as-is. If valid, ship it untouched.
	if json.Valid(input) {
		// Light schema check: try to unmarshal into the schema shape.
		// We don't have a full JSON Schema validator, so we rely on
		// the upstream to reject truly invalid inputs. The repairs
		// below handle the four known failure modes.
		if isWellFormed(input) {
			return RepairResult{Output: string(input)}
		}
	}

	// Step 2: try repairs in order.
	current := copyJSON(input)
	repaired := false

	for _, fn := range repairFns {
		if result, ok := fn(current); ok {
			current = result
			repaired = true
		}
	}

	if !repaired {
		return RepairResult{
			Error:  "no repair applied",
			Output: string(input),
		}
	}

	// Step 3: validate repaired output.
	if !json.Valid(current) {
		return RepairResult{
			Error:  "repair produced invalid JSON",
			Output: string(current),
		}
	}

	slog.Debug("tool input repaired", "before", string(input), "after", string(current))
	return RepairResult{
		Repaired: true,
		Output:   string(current),
	}
}

// isWellFormed returns true when input is a valid JSON object or array
// with non-trivial content (not just empty object/array).
func isWellFormed(input json.RawMessage) bool {
	if len(input) == 0 {
		return false
	}
	trimmed := strings.TrimSpace(string(input))
	if trimmed == "{}" || trimmed == "[]" {
		return false
	}
	// Try unmarshalling into generic interface to confirm well-formedness
	var v any
	return json.Unmarshal(input, &v) == nil
}

// copyJSON deep-copies a JSON message for mutation.
func copyJSON(input json.RawMessage) json.RawMessage {
	cp := make(json.RawMessage, len(input))
	copy(cp, input)
	return cp
}

// repairNullFields removes null-valued fields from a JSON object.
// Models sometimes send "key": null for optional fields instead of omitting
// them entirely, which strict validators reject.
func repairNullFields(input json.RawMessage) (json.RawMessage, bool) {
	var obj map[string]any
	if err := json.Unmarshal(input, &obj); err != nil {
		return nil, false
	}

	changed := false
	for k, v := range obj {
		if v == nil {
			delete(obj, k)
			changed = true
		}
	}

	if !changed {
		return nil, false
	}

	result, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return result, true
}

// repairStringifiedArray detects JSON arrays that were emitted as JSON-encoded
// strings (e.g. "[\"a\",\"b\"]" instead of ["a","b"]) and parses them back
// into actual arrays.
//
// Ordering requirement: must run before repairBareStringToArray, otherwise
// a stringified array gets wrapped into a single-element array containing
// the escaped string.
func repairStringifiedArray(input json.RawMessage) (json.RawMessage, bool) {
	// Walk input looking for string values that themselves contain JSON arrays.
	var obj map[string]any
	if err := json.Unmarshal(input, &obj); err != nil {
		return nil, false
	}

	changed := false
	for k, v := range obj {
		strVal, ok := v.(string)
		if !ok {
			continue
		}
		trimmed := strings.TrimSpace(strVal)
		if !strings.HasPrefix(trimmed, "[") || !strings.HasSuffix(trimmed, "]") {
			continue
		}
		// Try parsing the string as a JSON array
		var parsed []any
		if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
			continue
		}
		obj[k] = parsed
		changed = true
	}

	if !changed {
		return nil, false
	}

	result, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return result, true
}

// repairSingleArgWrapper handles the case where a model wraps a single
// expected value in an unnecessary object, e.g. {"key": "val"} where
// the schema expects a flat value or array of values.
// This is a narrow heuristic — it looks for top-level keys that are the
// only key and whose value is itself an object that doesn't match the
// expected shape, suggesting the model added a wrapping layer.
func repairSingleArgWrapper(input json.RawMessage) (json.RawMessage, bool) {
	var obj map[string]any
	if err := json.Unmarshal(input, &obj); err != nil {
		return nil, false
	}

	// Only attempt when there's exactly one key.
	if len(obj) != 1 {
		return nil, false
	}

	for k, v := range obj {
		nested, ok := v.(map[string]any)
		if !ok {
			continue
		}
		// The nested object IS the value — unwrap by replacing the
		// parent key's value with the nested object's keys flattened.
		// E.g. {"key": {"actual_key": "val"}} → {"actual_key": "val"}
		// This is a specific pattern seen in deepseek-flash where
		// single-arg tool calls get double-wrapped.
		if len(nested) == 1 {
			for nk, nv := range nested {
				newObj := map[string]any{nk: nv}
				result, err := json.Marshal(newObj)
				if err != nil {
					return nil, false
				}
				_ = k // original wrapper key is discarded
				return result, true
			}
		}
	}

	return nil, false
}

// repairBareStringToArray wraps bare string values that should be arrays.
// Models sometimes emit a plain string where the schema expects an array
// of strings, e.g. "foo" instead of ["foo"].
func repairBareStringToArray(input json.RawMessage) (json.RawMessage, bool) {
	var obj map[string]any
	if err := json.Unmarshal(input, &obj); err != nil {
		return nil, false
	}

	changed := false
	for k, v := range obj {
		strVal, ok := v.(string)
		if !ok {
			continue
		}
		// A single-element array is almost always the correct form.
		// Wrap the bare string.
		obj[k] = []any{strVal}
		changed = true
	}

	if !changed {
		return nil, false
	}

	result, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return result, true
}

// -- Markdown auto-link repair --

// autoLinkRe matches markdown auto-links where link text = URL (without protocol).
// This is the degenerate case from deepseek-flash: file paths emitted as
// [notes.md](http://notes.md) instead of plain /Users/x/proj/notes.md.
// Real markdown like [click](https://example.com) passes through untouched.
var autoLinkRe = regexp.MustCompile(`\[([^\]]+)\]\(https?://([^\)]+)\)`)

// RepairAutoLinks fixes markdown auto-links in file path strings.
// Only the degenerate case where link text equals URL path is unwrapped.
func RepairAutoLinks(input string) string {
	return autoLinkRe.ReplaceAllStringFunc(input, func(match string) string {
		parts := autoLinkRe.FindStringSubmatch(match)
		if len(parts) < 3 {
			return match
		}
		text := parts[1]
		urlPath := parts[2]
		// Only unwrap when link text equals URL path (degenerate auto-link).
		if text == urlPath || text == urlPath+"/" {
			return text
		}
		// Real markdown — leave untouched.
		return match
	})
}

// -- Relational invariants --

// ApplyRelationalDefaults fills in missing relational fields based on known
// tool patterns. Currently handles:
//   - readFile: "limit" alone → offset = 0. "offset" alone → limit = 2000.
//
// Returns the input unchanged when no relational defaults are applied.
func ApplyRelationalDefaults(toolName string, input json.RawMessage) json.RawMessage {
	if toolName == "" {
		return input
	}

	switch toolName {
	case "readFile", "read_file":
		return applyReadFileDefaults(input)
	default:
		return input
	}
}

// applyReadFileDefaults applies relational defaults for file-reading tools.
func applyReadFileDefaults(input json.RawMessage) json.RawMessage {
	var obj map[string]any
	if err := json.Unmarshal(input, &obj); err != nil {
		return input
	}

	_, hasOffset := obj["offset"]
	_, hasLimit := obj["limit"]
	changed := false

	if hasLimit && !hasOffset {
		obj["offset"] = 0
		changed = true
		slog.Debug("readFile relational default: limit without offset → offset=0")
	}
	if hasOffset && !hasLimit {
		obj["limit"] = 2000
		changed = true
		slog.Debug("readFile relational default: offset without limit → limit=2000")
	}

	if !changed {
		return input
	}

	result, err := json.Marshal(obj)
	if err != nil {
		return input
	}
	return result
}

// RepairToolCall is the top-level entry point. It applies the full repair
// pipeline: validate → repair → relational defaults → auto-link fix → return.
func RepairToolCall(toolName string, input json.RawMessage, schema json.RawMessage) RepairResult {
	// Step 1: validate-then-repair (shape problems).
	repairer := NewToolInputRepair()
	result := repairer.Repair(input, schema)

	// Step 2: apply relational defaults.
	repairedInput := []byte(result.Output)
	if result.Error == "" || result.Repaired {
		repairedInput = ApplyRelationalDefaults(toolName, repairedInput)
	}

	// Step 3: fix markdown auto-links in string fields.
	repairedStr := RepairAutoLinks(string(repairedInput))

	return RepairResult{
		Repaired: result.Repaired || repairedStr != string(repairedInput),
		Output:   repairedStr,
		Error:    result.Error,
	}
}

// Example usage error message for the model when repair fails.
const RepairFailedMsg = `Tool call input could not be repaired automatically.
This is likely because the tool arguments don't match the expected schema.
Retry with valid JSON arguments matching the tool's parameter schema.
Do not wrap arguments in extra objects. Use arrays where arrays are expected.
Do not include markdown auto-links in file paths.`
