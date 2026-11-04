package proto

type AppRuntimeChoice struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Version  string `json:"version,omitempty"`
	Source   string `json:"source"`
	Unit     string `json:"unit,omitempty"`
	State    string `json:"state,omitempty"`
	Current  bool   `json:"current,omitempty"`
	Disabled string `json:"disabled,omitempty"`
}

type AppRuntimeChoices struct {
	App       string             `json:"app"`
	Type      string             `json:"type"`
	CanSwitch bool               `json:"canSwitch"`
	Reason    string             `json:"reason,omitempty"`
	Port      string             `json:"port,omitempty"`
	Current   string             `json:"current,omitempty"`
	Choices   []AppRuntimeChoice `json:"choices"`
	Notes     []string           `json:"notes,omitempty"`
}
