package gates

import "fmt"

// Declaration is one resource a plan declares, and what it would run.
// internal/plan.Declarations builds these from `tofu show -json`'s
// configuration block, at every module depth; Address already carries any
// module prefix ("module.m.terraform_data.inner"), so this package needs no
// notion of nesting of its own.
type Declaration struct {
	Address      string
	Type         string
	Provisioners []string
}

// forbiddenTypes are resource types refused regardless of what they declare.
// A compiled-in slice, not configuration -- "one way to do things": there is
// nowhere else in this tree that could set this list, and a second knob for
// the same list is a second thing to keep in sync.
//
// docs/work-items.md's "`helm_release` had no enforcer on the tofu side"
// recorded that a helm_release resource reaching a chart repository at
// apply time had nothing looking for it. This gate is what closed it.
var forbiddenTypes = []string{
	"helm_release",
}

// CheckDeclarations refuses a plan that would execute code the diff does
// not show.
//
// A provisioner runs an arbitrary command at apply time, on the machine
// holding write credentials for four clouds, and the plan carries no diff
// of what that command will do -- only the resource's own attribute change,
// if it has one. Approving the plan approves the resource; it cannot also
// be approving the command, because the command was never shown to whoever
// approved it. So every provisioner is refused, by address and type,
// whether or not the resource carrying it has any change this run: a
// resource untouched THIS pass still has its provisioner ready to fire the
// next time anything touches it.
//
// A forbidden resource type is refused for the same underlying reason a
// provisioner is: a chart is content fetched at apply time, not a diff the
// reviewer saw.
//
// Every offender is reported, not just the first -- an operator fixing a
// plan one refusal at a time, re-running the whole apply between each, is
// the outcome this function exists to avoid.
func CheckDeclarations(root string, decls []Declaration) []string {
	var problems []string
	for _, d := range decls {
		for _, p := range d.Provisioners {
			problems = append(problems, fmt.Sprintf(
				"%s: %s declares provisioner %q, which runs at apply time with no diff of what it will do",
				root, d.Address, p))
		}
		for _, forbidden := range forbiddenTypes {
			if d.Type == forbidden {
				problems = append(problems, fmt.Sprintf(
					"%s: %s is a forbidden resource type %q", root, d.Address, d.Type))
			}
		}
	}
	return problems
}
