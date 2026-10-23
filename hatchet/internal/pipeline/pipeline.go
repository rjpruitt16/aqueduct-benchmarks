// Package pipeline holds the types shared by the benchmark's worker and
// driver.
package pipeline

// WorkflowName is the Hatchet workflow that processes one document.
const WorkflowName = "document"

// Document is the workflow input: one document from one tenant.
type Document struct {
	DocID  string `json:"doc_id"`
	Tenant string `json:"tenant"`
	Pages  int    `json:"pages"`
}

// Completion is what the index step POSTs to the driver.
type Completion struct {
	DocID string `json:"doc_id"`
	RunID string `json:"run_id"`
}
