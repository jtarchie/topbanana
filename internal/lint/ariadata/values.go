package ariadata

import (
	"strconv"
	"strings"
)

// ValueType is how an aria-* attribute's value is constrained.
type ValueType int

const (
	// ValString is unconstrained free text, so there is nothing to validate.
	ValString ValueType = iota
	ValBoolean
	// ValBooleanUndefined is true/false/undefined, where undefined means "not applicable".
	ValBooleanUndefined
	ValTristate
	ValToken
	// ValTokenList is space-separated, every token from the same set.
	ValTokenList
	ValInteger
	ValNumber
	// ValIDRef and ValIDRefList are resolved against the page by internal/lint, not here.
	ValIDRef
	ValIDRefList
)

type valueSpec struct {
	Type   ValueType
	Tokens []string
}

// Hand-written because roles.json carries no value grammar; it is the one table here not derived from it.
var valueSpecs = map[string]valueSpec{
	"aria-activedescendant":       {Type: ValIDRef},
	"aria-atomic":                 {Type: ValBoolean},
	"aria-autocomplete":           {Type: ValToken, Tokens: []string{"inline", "list", "both", "none"}},
	"aria-braillelabel":           {Type: ValString},
	"aria-brailleroledescription": {Type: ValString},
	"aria-busy":                   {Type: ValBoolean},
	"aria-checked":                {Type: ValTristate},
	"aria-colcount":               {Type: ValInteger},
	"aria-colindex":               {Type: ValInteger},
	"aria-colspan":                {Type: ValInteger},
	"aria-controls":               {Type: ValIDRefList},
	"aria-current":                {Type: ValToken, Tokens: []string{"page", "step", "location", "date", "time", "true", "false"}},
	"aria-describedby":            {Type: ValIDRefList},
	"aria-description":            {Type: ValString},
	"aria-details":                {Type: ValIDRefList},
	"aria-disabled":               {Type: ValBoolean},
	"aria-dropeffect":             {Type: ValTokenList, Tokens: []string{"copy", "execute", "link", "move", "none", "popup"}},
	"aria-errormessage":           {Type: ValIDRef},
	"aria-expanded":               {Type: ValBooleanUndefined},
	"aria-flowto":                 {Type: ValIDRefList},
	"aria-grabbed":                {Type: ValBooleanUndefined},
	"aria-haspopup":               {Type: ValToken, Tokens: []string{"false", "true", "menu", "listbox", "tree", "grid", "dialog"}},
	"aria-hidden":                 {Type: ValBooleanUndefined},
	"aria-invalid":                {Type: ValToken, Tokens: []string{"false", "true", "grammar", "spelling"}},
	"aria-keyshortcuts":           {Type: ValString},
	"aria-label":                  {Type: ValString},
	"aria-labelledby":             {Type: ValIDRefList},
	"aria-level":                  {Type: ValInteger},
	"aria-live":                   {Type: ValToken, Tokens: []string{"off", "polite", "assertive"}},
	"aria-modal":                  {Type: ValBoolean},
	"aria-multiline":              {Type: ValBoolean},
	"aria-multiselectable":        {Type: ValBoolean},
	"aria-orientation":            {Type: ValToken, Tokens: []string{"horizontal", "vertical", "undefined"}},
	"aria-owns":                   {Type: ValIDRefList},
	"aria-placeholder":            {Type: ValString},
	"aria-posinset":               {Type: ValInteger},
	"aria-pressed":                {Type: ValTristate},
	"aria-readonly":               {Type: ValBoolean},
	"aria-relevant":               {Type: ValTokenList, Tokens: []string{"additions", "all", "removals", "text"}},
	"aria-required":               {Type: ValBoolean},
	"aria-roledescription":        {Type: ValString},
	"aria-rowcount":               {Type: ValInteger},
	"aria-rowindex":               {Type: ValInteger},
	"aria-rowspan":                {Type: ValInteger},
	"aria-selected":               {Type: ValBooleanUndefined},
	"aria-setsize":                {Type: ValInteger},
	"aria-sort":                   {Type: ValToken, Tokens: []string{"ascending", "descending", "none", "other"}},
	"aria-valuemax":               {Type: ValNumber},
	"aria-valuemin":               {Type: ValNumber},
	"aria-valuenow":               {Type: ValNumber},
	"aria-valuetext":              {Type: ValString},
}

// ValueOf reports how an attribute's value is constrained; unknown attributes read as free text.
func ValueOf(attr string) (ValueType, []string) {
	spec, ok := valueSpecs[attr]
	if !ok {
		return ValString, nil
	}
	return spec.Type, spec.Tokens
}

// ID references always pass here — only the page knows whether an id exists.
func CheckValue(attr, value string) (ok bool, allowed []string) {
	kind, tokens := ValueOf(attr)
	v := strings.TrimSpace(value)

	switch kind {
	case ValString, ValIDRef, ValIDRefList:
		return true, nil
	case ValBoolean:
		return oneOf(v, "true", "false"), []string{"true", "false"}
	case ValBooleanUndefined:
		return oneOf(v, "true", "false", "undefined"), []string{"true", "false", "undefined"}
	case ValTristate:
		return oneOf(v, "true", "false", "mixed", "undefined"), []string{"true", "false", "mixed", "undefined"}
	case ValToken:
		return oneOf(v, tokens...), tokens
	case ValTokenList:
		fields := strings.Fields(v)
		if len(fields) == 0 {
			return false, tokens
		}
		for _, f := range fields {
			if !oneOf(f, tokens...) {
				return false, tokens
			}
		}
		return true, tokens
	case ValInteger:
		_, err := strconv.Atoi(v)
		return err == nil, []string{"a whole number"}
	case ValNumber:
		_, err := strconv.ParseFloat(v, 64)
		return err == nil, []string{"a number"}
	}
	return true, nil
}

// IsIDRef reports whether the attribute's value names other elements by id.
func IsIDRef(attr string) (list bool, isRef bool) {
	kind, _ := ValueOf(attr)
	switch kind {
	case ValIDRef:
		return false, true
	case ValIDRefList:
		return true, true
	case ValString, ValBoolean, ValBooleanUndefined, ValTristate, ValToken, ValTokenList, ValInteger, ValNumber:
		return false, false
	}
	return false, false
}

func oneOf(v string, allowed ...string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}
