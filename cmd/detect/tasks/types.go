package tasks

import (
	"github.com/evcc-io/evcc/util"
)

type ResultDetails struct {
	IP              string
	Port            int              `json:",omitempty"`
	Topic           string           `json:",omitempty"`
	ModbusResult    *ModbusResult    `json:",omitempty"`
	KebaResult      *KebaResult      `json:",omitempty"`
	SmaResult       *SmaResult       `json:",omitempty"`
	IaqualinkResult *IaqualinkResult `json:",omitempty"`
}

// IaqualinkResult holds discovery output for IAquaLink local HTTP API.
type IaqualinkResult struct {
	BaseURL    string `json:"baseUrl"`
	Mode       int64  `json:"mode"`
	MatchedURL string `json:"matchedUrl,omitempty"`
}

type Result struct {
	Task
	ResultDetails
	Attributes map[string]any // TODO remove, only used for post-processing
}

type TaskType string

type Task struct {
	ID      string
	Type    TaskType
	Depends string
	Config  map[string]any
	TaskHandler
}

type TaskHandler interface {
	Test(log *util.Logger, in ResultDetails) []ResultDetails
}
