package tui

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type persistedState struct {
	Page               Page   `json:"page"`
	Project            string `json:"project,omitempty"`
	ExcludeProject     string `json:"exclude_project,omitempty"`
	Agent              string `json:"agent,omitempty"`
	ExcludeAgent       string `json:"exclude_agent,omitempty"`
	Machine            string `json:"machine,omitempty"`
	Model              string `json:"model,omitempty"`
	ExcludeModel       string `json:"exclude_model,omitempty"`
	GitBranch          string `json:"git_branch,omitempty"`
	Date               string `json:"date,omitempty"`
	From               string `json:"from,omitempty"`
	To                 string `json:"to,omitempty"`
	ActiveSince        string `json:"active_since,omitempty"`
	Termination        string `json:"termination,omitempty"`
	Outcome            string `json:"outcome,omitempty"`
	HealthGrade        string `json:"health_grade,omitempty"`
	OrderBy            string `json:"order_by,omitempty"`
	MinMessages        int    `json:"min_messages,omitempty"`
	MaxMessages        int    `json:"max_messages,omitempty"`
	MinUserMessages    int    `json:"min_user_messages,omitempty"`
	MinToolFailures    *int   `json:"min_tool_failures,omitempty"`
	HasSecret          bool   `json:"has_secret,omitempty"`
	Starred            bool   `json:"starred,omitempty"`
	Terms              string `json:"terms,omitempty"`
	CompareDimension   string `json:"compare_dimension,omitempty"`
	CompareLeft        string `json:"compare_left,omitempty"`
	CompareRight       string `json:"compare_right,omitempty"`
	IncludeOneShot     bool   `json:"include_one_shot,omitempty"`
	IncludeAutomated   bool   `json:"include_automated,omitempty"`
	IncludeChildren    bool   `json:"include_children,omitempty"`
	ActivityPreset     string `json:"activity_preset,omitempty"`
	ActivityDate       string `json:"activity_date,omitempty"`
	ActivityBucket     string `json:"activity_bucket,omitempty"`
	ActivityAutomation string `json:"activity_automation,omitempty"`
	TrendGranularity   string `json:"trend_granularity,omitempty"`
	InsightType        string `json:"insight_type,omitempty"`
	Theme              string `json:"theme,omitempty"`
	HighContrast       bool   `json:"high_contrast,omitempty"`
	MessageLayout      string `json:"message_layout,omitempty"`
	HideThinking       bool   `json:"hide_thinking,omitempty"`
	HideTools          bool   `json:"hide_tools,omitempty"`
}

func loadState(path string) persistedState {
	var state persistedState
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &state) != nil {
		return state
	}
	if !validPage(state.Page) {
		state.Page = PageSessions
	}
	return state
}

func saveState(path string, state persistedState) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tui-state-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			_ = os.Remove(path)
			return os.Rename(tmpName, path)
		}
		return err
	}
	return nil
}

func validPage(page Page) bool {
	for _, candidate := range pages {
		if page == candidate {
			return true
		}
	}
	return false
}
