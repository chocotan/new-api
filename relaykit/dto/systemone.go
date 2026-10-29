package dto

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"strconv"

	"github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/tidwall/sjson"
)

// SystemOneRequest preserves the order of state, questions and criteria. Their
// structure is part of the decision input, rather than a Chat message.
type SystemOneRequest struct {
	Model     string                       `json:"model"`
	State     json.RawMessage              `json:"state"`
	Questions map[string]SystemOneQuestion `json:"questions"`
	raw       []byte
}
type SystemOneQuestion struct {
	Type         string          `json:"type"`
	Instructions json.RawMessage `json:"instructions"`
	Criteria     json.RawMessage `json:"criteria"`
}

func (r *SystemOneRequest) UnmarshalJSON(data []byte) error {
	type request SystemOneRequest
	var value request
	if err := kitutil.Unmarshal(data, &value); err != nil {
		return err
	}
	*r = SystemOneRequest(value)
	r.raw = append([]byte(nil), data...)
	return nil
}
func (r SystemOneRequest) MarshalJSON() ([]byte, error) {
	return sjson.SetBytes(r.raw, "model", r.Model)
}
func (r *SystemOneRequest) SetModelName(model string)   { r.Model = model }
func (r *SystemOneRequest) IsStream(*http.Request) bool { return false }
func (r *SystemOneRequest) GetTokenCountMeta() *types.TokenCountMeta {
	return &types.TokenCountMeta{TokenType: types.TokenTypeTokenizer, CombineText: string(r.raw)}
}
func (r *SystemOneRequest) Validate() error {
	var fields map[string]json.RawMessage
	if kitutil.Unmarshal(r.raw, &fields) != nil || r.Model == "" || len(r.Questions) == 0 {
		return errors.New("model and nonempty questions are required")
	}
	for key := range fields {
		switch key {
		case "model", "state", "questions", "user", "session_id", "trace", "provider":
		default:
			return fmt.Errorf("unsupported System One field %q", key)
		}
	}
	if len(r.State) == 0 || string(r.State) == "null" || r.State[0] != '"' && r.State[0] != '{' && r.State[0] != '[' {
		return errors.New("state must be a string, object or array")
	}
	for _, q := range r.Questions {
		if len(q.Instructions) == 0 || string(q.Instructions) == "null" {
			return errors.New("question instructions are required")
		}
		switch q.Type {
		case "choice":
			var criteria map[string]json.RawMessage
			if kitutil.Unmarshal(q.Criteria, &criteria) != nil || len(criteria) == 0 {
				return errors.New("choice criteria must be a nonempty object")
			}
		case "score":
			var criteria []json.RawMessage
			if kitutil.Unmarshal(q.Criteria, &criteria) != nil || len(criteria) == 0 {
				return errors.New("score criteria must be a nonempty array")
			}
		case "noul":
			if len(q.Criteria) > 0 && string(q.Criteria) != "null" {
				var criteria map[string]json.RawMessage
				if kitutil.Unmarshal(q.Criteria, &criteria) != nil || len(criteria) != 2 || criteria["true"] == nil || criteria["false"] == nil {
					return errors.New("noul criteria must contain true and false")
				}
			}
		default:
			return errors.New("unsupported System One question type")
		}
	}
	return nil
}

// ValidateResponse checks the typed contract without projecting or rewriting it.
// Cost is deliberately validated independently by the host billing mechanism.
func (r *SystemOneRequest) ValidateResponse(data []byte) (*Usage, error) {
	var response struct {
		Model   string `json:"model"`
		Answers map[string]struct {
			Type          string                     `json:"type"`
			Choice        *string                    `json:"choice"`
			Noul          *float64                   `json:"noul"`
			Score         *float64                   `json:"score"`
			Probabilities map[string]*float64        `json:"probabilities"`
			Confidence    *float64                   `json:"confidence"`
			Legend        map[string]json.RawMessage `json:"legend"`
		} `json:"answers"`
		Usage struct {
			Input  *int `json:"input_tokens"`
			Output *int `json:"output_tokens"`
		} `json:"usage"`
	}
	invalid := errors.New("invalid System One typed response")
	if kitutil.Unmarshal(data, &response) != nil || response.Model == "" || len(response.Answers) != len(r.Questions) {
		return nil, invalid
	}
	for key, q := range r.Questions {
		a, ok := response.Answers[key]
		if !ok || a.Type != q.Type {
			return nil, invalid
		}
		var options map[string]json.RawMessage
		switch q.Type {
		case "choice":
			_ = kitutil.Unmarshal(q.Criteria, &options)
			if a.Choice == nil || options[*a.Choice] == nil {
				return nil, invalid
			}
		case "noul":
			if a.Noul == nil || !unitInterval(*a.Noul) {
				return nil, invalid
			}
			options = map[string]json.RawMessage{"true": nil, "false": nil}
		case "score":
			var criteria []json.RawMessage
			_ = kitutil.Unmarshal(q.Criteria, &criteria)
			if a.Score == nil || math.IsNaN(*a.Score) || math.IsInf(*a.Score, 0) || *a.Score < 0 || *a.Score > float64(len(criteria)-1) {
				return nil, invalid
			}
			options = make(map[string]json.RawMessage, len(criteria))
			for i, criterion := range criteria {
				options[strconv.Itoa(i)] = criterion
			}
		}
		if a.Confidence != nil && !unitInterval(*a.Confidence) {
			return nil, invalid
		}
		if a.Probabilities != nil {
			if len(a.Probabilities) != len(options) {
				return nil, invalid
			}
			for option, p := range a.Probabilities {
				if _, ok := options[option]; !ok || p == nil || !unitInterval(*p) {
					return nil, invalid
				}
			}
		}
		if a.Legend != nil {
			if q.Type != "score" || len(a.Legend) != len(options) {
				return nil, invalid
			}
			for option, actual := range a.Legend {
				expected, ok := options[option]
				if !ok {
					return nil, invalid
				}
				var actualValue, expectedValue any
				if kitutil.Unmarshal(actual, &actualValue) != nil || kitutil.Unmarshal(expected, &expectedValue) != nil || !reflect.DeepEqual(actualValue, expectedValue) {
					return nil, invalid
				}
			}
		}
	}
	if response.Usage.Input == nil || response.Usage.Output == nil || *response.Usage.Input < 0 || *response.Usage.Output < 0 || *response.Usage.Input > math.MaxInt32 || *response.Usage.Output > math.MaxInt32 || int64(*response.Usage.Input)+int64(*response.Usage.Output) > math.MaxInt32 {
		return nil, invalid
	}
	return &Usage{PromptTokens: *response.Usage.Input, CompletionTokens: *response.Usage.Output, TotalTokens: *response.Usage.Input + *response.Usage.Output}, nil
}
func unitInterval(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 1 }
