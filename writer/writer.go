package writer

import (
	"io"

	"github.com/laredoute/tfreport/terraformstate"
	tfjson "github.com/hashicorp/terraform-json"
)

// Writer writes formatted Terraform plan output.
type Writer interface {
	Write(writer io.Writer) error
}

// NewReportWriter returns a Writer that always renders the full details report.
// This is the primary entry point for tfreport.
func NewReportWriter(plan tfjson.Plan) Writer {
	pv := terraformstate.BuildPlannedValuesMap(plan)
	changes := terraformstate.GetAllResourceChanges(plan)
	outputs := terraformstate.GetAllOutputChanges(plan)
	return NewTableWriter(changes, outputs, false, true, pv)
}
