// Package work holds the messages the coordinator and the agents exchange,
// and the record of every resource call a workflow made.
package work

// Task is the coordinator's input: one workflow to run.
type Task struct {
	Workflow string `json:"workflow"`
	Issue    string `json:"issue"`
	Tenant   string `json:"tenant"`
}

// Step is the input to a specialist agent.
type Step struct {
	Workflow   string `json:"workflow"`
	Issue      string `json:"issue"`
	Tenant     string `json:"tenant"`
	Round      int    `json:"round"`
	Diagnosis  string `json:"diagnosis,omitempty"`
	TestOutput string `json:"test_output,omitempty"` // the last failing run, for the next round
	Fix        string `json:"fix,omitempty"`
}

// CallRecord is one resource call, as the calling agent saw it.
type CallRecord struct {
	Resource  string `json:"resource"` // llm, db, sandbox or ci
	Step      string `json:"step"`
	Key       string `json:"key"`
	Status    int    `json:"status"`
	Attempts  int    `json:"attempts"`
	BackoffMs int64  `json:"backoff_ms"`
	Ms        int64  `json:"ms"`
	Tokens    int    `json:"tokens,omitempty"` // generated, for llm calls
}

// Research is the research agent's reply.
type Research struct {
	Diagnosis string       `json:"diagnosis"`
	Calls     []CallRecord `json:"calls"`
}

// Coding is the coding agent's reply.
type Coding struct {
	Fix   string       `json:"fix"`
	Calls []CallRecord `json:"calls"`
}

// Verify is the verification agent's reply.
type Verify struct {
	Passed      bool         `json:"passed"`
	ExecutionID string       `json:"execution_id"`
	Output      string       `json:"output"`
	CIRunID     string       `json:"ci_run_id,omitempty"` // the CI run that passed, when CI is on
	CIAttempts  int          `json:"ci_attempts,omitempty"`
	Calls       []CallRecord `json:"calls"`
}

// Result is the coordinator's final artifact.
type Result struct {
	Workflow    string       `json:"workflow"`
	Issue       string       `json:"issue"`
	Tenant      string       `json:"tenant"`
	Verified    bool         `json:"verified"`
	Fix         string       `json:"fix"`
	ExecutionID string       `json:"execution_id"` // the sandbox run that verified it
	CIRunID     string       `json:"ci_run_id,omitempty"`
	Rounds      int          `json:"rounds"`
	Error       string       `json:"error,omitempty"`
	Calls       []CallRecord `json:"calls"`
}
