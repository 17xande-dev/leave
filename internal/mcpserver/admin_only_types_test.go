package mcpserver

import (
	"context"
	"strings"
	"testing"
)

func TestApplyToolDescriptionExplainsAdminOnlyTypes(t *testing.T) {
	f := newFixture(t)
	tools, err := f.as("staff1").ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	const sentence = "Some leave types (for example compassionate and unpaid leave) are arranged with HR and cannot be applied for; my_balances lists the ones you can."
	for _, tool := range tools.Tools {
		if tool.Name == "apply_for_leave" {
			if !strings.Contains(tool.Description, sentence) {
				t.Fatalf("apply_for_leave description lost HR guidance: %q", tool.Description)
			}
			return
		}
	}
	t.Fatal("apply_for_leave tool missing")
}
