// The release gate retains complete native stake/permit regressions and the
// actual fail-closed production startup edge, not merely standalone helpers.
package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"
)

// Both source groups must remain completely selected by the exact runtime
// client prefix used by the existing launch-critical producer gate.
func TestProducerGatePinsExactBlockRuntimeClientRegressionsNativeStakeCoverage(t *testing.T) {
	for _, path := range []string{"../crv4/validator_stake_test.go", "../validator/release_native_validator_test.go"} {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := verifyReleaseSourceTestCoverage(releaseRuntimeClientSelector, "^Test", []string{string(source)}); err != nil {
			t.Fatalf("native stake coverage %s: %v", path, err)
		}
	}
}

// Every entry point must forward to the same startup body. Its ordinary branch
// tests the signing hotkey after exact EVM registration and returns on refusal
// before recovery, state loading or any worker launch. A retained activation
// may supply the UID hint, but cannot skip the current native stake check.
func TestProducerGatePinsExactBlockRuntimeClientRegressionsNativeStakeStartup(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "../validator/release_run.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	functions := map[string]*ast.FuncDecl{}
	for _, declaration := range file.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok {
			functions[function.Name.Name] = function
		}
	}
	for _, edge := range []struct {
		from, to string
		args     []string
	}{
		{from: "RunRelease", to: "runReleaseWithActivationSetup", args: []string{"ctx", "configPath", "nil"}},
		{from: "runReleaseWithActivationSetup", to: "runReleaseWithStartupV2", args: []string{"ctx", "configPath", "retainedSetup", "nil"}},
		{from: "runReleaseWithStartupV2", to: "runReleaseWithStartupAndProgressV2", args: []string{"ctx", "configPath", "retainedSetup", "adoption", "nil", `""`}},
		{from: "RunReleaseWithProgress", to: "runReleaseWithStartupAndProgressV2", args: []string{"ctx", "configPath", "nil", "nil", "nil", "progressPath"}},
		{from: "RunReleaseWithNativeGapBridge", to: "runReleaseWithStartupAndProgressV2", args: []string{"ctx", "configPath", "nil", "nil", "bridge", "progressPath"}},
	} {
		function := functions[edge.from]
		if function == nil || function.Body == nil || len(function.Body.List) != 1 {
			t.Fatalf("production entry %s is not one direct forwarding return", edge.from)
		}
		statement, ok := function.Body.List[0].(*ast.ReturnStmt)
		if !ok || len(statement.Results) != 1 {
			t.Fatalf("production entry %s does not return its startup result", edge.from)
		}
		call, ok := statement.Results[0].(*ast.CallExpr)
		if !ok || len(call.Args) != len(edge.args) {
			t.Fatalf("production entry %s changed its startup call", edge.from)
		}
		target, ok := call.Fun.(*ast.Ident)
		if !ok || target.Name != edge.to {
			t.Fatalf("production entry %s bypasses %s", edge.from, edge.to)
		}
		for index, expected := range edge.args {
			argument := ""
			switch value := call.Args[index].(type) {
			case *ast.Ident:
				argument = value.Name
			case *ast.BasicLit:
				argument = value.Value
			}
			if argument != expected {
				t.Fatalf("production entry %s changed argument %d from %s", edge.from, index, expected)
			}
		}
	}
	run := functions["runReleaseWithStartupAndProgressV2"]
	if run == nil || run.Body == nil {
		t.Fatal("real release startup body is absent")
	}
	positions := map[string][]token.Pos{}
	calls := map[string][]*ast.CallExpr{}
	ast.Inspect(run.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := ""
		switch callee := call.Fun.(type) {
		case *ast.Ident:
			name = callee.Name
		case *ast.SelectorExpr:
			name = callee.Sel.Name
			if owner, ok := callee.X.(*ast.Ident); ok {
				qualified := owner.Name + "." + name
				positions[qualified] = append(positions[qualified], call.Pos())
			}
		}
		positions[name] = append(positions[name], call.Pos())
		calls[name] = append(calls[name], call)
		return true
	})
	registration, hint, stake := positions["FindUidByHotkeyAtHashContext"], positions["findProvisionalValidatorUIDAtHashContext"], positions["authenticateReleaseValidatorStakeContext"]
	if len(registration) != 1 || len(hint) != 1 || len(stake) != 1 || registration[0] >= stake[0] || hint[0] >= stake[0] {
		t.Fatal("native startup stake admission is absent, duplicated or before EVM registration")
	}
	// The stake admission is the else branch of the single production split.
	// Everything after that statement runs after it on the ordinary path.
	var startup *ast.IfStmt
	var ordinary *ast.BlockStmt
	for _, statement := range run.Body.List {
		conditional, ok := statement.(*ast.IfStmt)
		if !ok {
			continue
		}
		condition, ok := conditional.Cond.(*ast.Ident)
		alternative, alternativeOK := conditional.Else.(*ast.BlockStmt)
		if ok && condition.Name == "production" && alternativeOK && stake[0] > alternative.Pos() && stake[0] < alternative.End() {
			if startup != nil {
				t.Fatal("ordinary startup stake admission has more than one production split")
			}
			startup, ordinary = conditional, alternative
		}
	}
	if startup == nil {
		t.Fatal("native startup stake admission is not the ordinary branch of the production split")
	}
	var activation []token.Pos
	for _, position := range positions["loadReleaseEvidenceV2ActivationInputsWithRetainedSetup"] {
		if position > ordinary.Pos() && position < ordinary.End() {
			activation = append(activation, position)
		}
	}
	if len(activation) != 2 || activation[0] >= hint[0] || activation[1] <= stake[0] {
		t.Fatal("retained UID source or strict post-stake activation ordering changed")
	}
	// The sole pre-stake activation read belongs only to retained startup.
	// Ordinary startup must still authenticate the direct current EVM lookup
	// and native stake before loading its activation sources.
	for _, edge := range []struct {
		position token.Pos
		op       token.Token
	}{
		{position: activation[0], op: token.NEQ},
		{position: activation[1], op: token.EQL},
		{position: hint[0], op: token.NEQ},
	} {
		matched := 0
		for _, statement := range ordinary.List {
			conditional, ok := statement.(*ast.IfStmt)
			if !ok || edge.position <= conditional.Body.Pos() || edge.position >= conditional.Body.End() {
				continue
			}
			condition, ok := conditional.Cond.(*ast.BinaryExpr)
			if !ok || condition.Op != edge.op {
				continue
			}
			left, leftOK := condition.X.(*ast.Ident)
			right, rightOK := condition.Y.(*ast.Ident)
			if !leftOK || left.Name != "retainedSetup" || !rightOK || right.Name != "nil" {
				continue
			}
			if edge.position == hint[0] {
				alternative, ok := conditional.Else.(*ast.BlockStmt)
				if !ok || registration[0] <= alternative.Pos() || registration[0] >= alternative.End() {
					t.Fatal("strict startup no longer selects the direct finalized EVM registration lookup")
				}
			}
			matched++
		}
		if matched != 1 {
			t.Fatal("activation/UID source read is not scoped to its exact startup mode")
		}
	}
	// Recovery now belongs to the authenticated V2 runtime constructor. Pin
	// every real disk/semantic/worker edge after the same signing-key check.
	for _, operation := range []string{
		"newReleaseEvidenceV2StartupReaders",
		"readReleaseServerKeysV2", "openReleaseEvidenceV2DiskState", "newReleaseRuntimeV2",
		"startReleaseOperator", "runtimeV2.attach", "newReleaseSteererV2",
		"advanceInitialReleaseWithRetry", "runReleaseSettlementRefresh", "runReleaseOperatorWorkers",
	} {
		if len(positions[operation]) == 0 {
			t.Fatalf("startup operation %s disappeared without updating the custody guard", operation)
		}
		for _, position := range positions[operation] {
			if position <= startup.End() {
				t.Fatalf("startup operation %s precedes native stake admission", operation)
			}
		}
	}
	// advance is passed as the callback of both actual invocation owners.
	// Merely mentioning the method elsewhere must not satisfy this edge.
	for _, operation := range []string{"advanceInitialReleaseWithRetry", "runReleaseSettlementRefresh"} {
		owned := calls[operation]
		if len(owned) != 1 || len(owned[0].Args) <= 3 {
			t.Fatalf("startup advance owner %s changed", operation)
		}
		callback, ok := owned[0].Args[3].(*ast.SelectorExpr)
		if !ok || callback.Sel.Name != "advance" {
			t.Fatalf("startup advance owner %s lost its runtime callback", operation)
		}
		owner, ok := callback.X.(*ast.Ident)
		if !ok || owner.Name != "runtimeV2" {
			t.Fatalf("startup advance owner %s changed its runtime", operation)
		}
	}
	guarded := 0
	for _, statement := range ordinary.List {
		conditional, ok := statement.(*ast.IfStmt)
		if !ok {
			continue
		}
		assignment, ok := conditional.Init.(*ast.AssignStmt)
		if !ok || len(assignment.Lhs) != 2 || len(assignment.Rhs) != 1 {
			continue
		}
		call, ok := assignment.Rhs[0].(*ast.CallExpr)
		if !ok || len(call.Args) != 5 {
			continue
		}
		callee, ok := call.Fun.(*ast.Ident)
		if !ok || callee.Name != "authenticateReleaseValidatorStakeContext" {
			continue
		}
		for _, expected := range []struct {
			index int
			name  string
		}{{index: 0, name: "ctx"}, {index: 1, name: "native"}, {index: 2, name: "cfg"}, {index: 4, name: "validatorUID"}} {
			argument, ok := call.Args[expected.index].(*ast.Ident)
			if !ok || argument.Name != expected.name {
				t.Fatalf("native startup argument %d is not %s", expected.index, expected.name)
			}
		}
		keyCall, ok := call.Args[3].(*ast.CallExpr)
		if !ok || len(keyCall.Args) != 0 {
			t.Fatal("native startup did not use the actual signing public key")
		}
		keyMethod, ok := keyCall.Fun.(*ast.SelectorExpr)
		if !ok || keyMethod.Sel.Name != "PublicKey" {
			t.Fatal("native startup signing method changed")
		}
		keyOwner, ok := keyMethod.X.(*ast.Ident)
		if !ok || keyOwner.Name != "hotkey" {
			t.Fatal("native startup signing owner changed")
		}
		condition, ok := conditional.Cond.(*ast.BinaryExpr)
		if !ok || condition.Op != token.NEQ {
			t.Fatal("native startup error is not tested")
		}
		left, leftOK := condition.X.(*ast.Ident)
		right, rightOK := condition.Y.(*ast.Ident)
		if !leftOK || left.Name != "err" || !rightOK || right.Name != "nil" {
			t.Fatal("native startup condition does not reject err != nil")
		}
		if len(conditional.Body.List) != 1 {
			t.Fatal("native startup refusal does not return directly")
		}
		refusal, ok := conditional.Body.List[0].(*ast.ReturnStmt)
		if !ok || len(refusal.Results) != 1 {
			t.Fatal("native startup refusal is not returned")
		}
		returned, ok := refusal.Results[0].(*ast.Ident)
		if !ok || returned.Name != "err" {
			t.Fatal("native startup refusal is discarded")
		}
		guarded++
	}
	if guarded != 1 {
		t.Fatal("real release startup lacks exactly one fail-closed native stake admission")
	}
	// Since d1808f79 a production (owner-recycle) config takes the other branch:
	// it recovers history, opens disk state and launches its workers before any
	// current UID or stake check. Its refresh worker checks both once, before
	// its first advance, and only then closes the readiness that trails and
	// fresh intents wait for.
	if len(positions["runProductionPreparationAndRefresh"]) != 1 {
		t.Fatal("production startup lost its preparation refresh worker")
	}
	readinessAssigned, readinessPassed := false, false
	ast.Inspect(run.Body, func(node ast.Node) bool {
		switch value := node.(type) {
		case *ast.AssignStmt:
			if len(value.Lhs) == 1 && len(value.Rhs) == 1 && releaseStartupName(value.Lhs[0]) == "trailReady" && releaseStartupName(value.Rhs[0]) == "runtimeV2.preparation.ready" {
				readinessAssigned = true
			}
		case *ast.KeyValueExpr:
			if releaseStartupName(value.Key) == "trailReady" && releaseStartupName(value.Value) == "trailReady" {
				readinessPassed = true
			}
		}
		return true
	})
	if !readinessAssigned || !readinessPassed {
		t.Fatal("production trails no longer wait for the preparation readiness")
	}
	for _, order := range []struct {
		path, function string
		steps          []string
	}{
		{path: "../validator/production_startup.go", function: "runProductionPreparationAndRefresh", steps: []string{"FindUidByHotkeyAtHashContext", "authenticateReleaseValidatorStakeContext", "advance", "close"}},
		{path: "../validator/release_shutdown.go", function: "runReleaseOperatorWorkers", steps: []string{"<-operations.trailReady", "reportReleaseTrailEngineError"}},
		{path: "../validator/release_submit_v2.go", function: "submitOnceV2", steps: []string{"requestProductionPreparation", "PrepareWeightsCRv4ExactAtContext", "beginV2", "submitPreparedNativeRuntimeContext"}},
	} {
		function := releaseStartupFunction(t, order.path, order.function)
		steps := releaseStartupSteps(function)
		var previous token.Pos
		for _, step := range order.steps {
			if len(steps[step]) != 1 || steps[step][0] <= previous {
				t.Fatalf("%s lost its production readiness order at %s", order.function, step)
			}
			previous = steps[step][0]
		}
	}
	// Readiness closes only on the iteration whose stake check and advance
	// both succeeded; a fresh intent proceeds only once readiness has closed.
	refresh := releaseStartupFunction(t, "../validator/production_startup.go", "runProductionPreparationAndRefresh")
	closesOnSuccess := false
	ast.Inspect(refresh.Body, func(node ast.Node) bool {
		conditional, ok := node.(*ast.IfStmt)
		if !ok || conditional.Else == nil {
			return true
		}
		condition, ok := conditional.Cond.(*ast.BinaryExpr)
		if !ok || condition.Op != token.NEQ || releaseStartupName(condition.X) != "err" || releaseStartupName(condition.Y) != "nil" {
			return true
		}
		ast.Inspect(conditional.Else, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if ok && releaseStartupName(call.Fun) == "close" && len(call.Args) == 1 && releaseStartupName(call.Args[0]) == "runtime.preparation.ready" {
				closesOnSuccess = true
			}
			return true
		})
		return true
	})
	if !closesOnSuccess {
		t.Fatal("production readiness can close without a successful stake check and advance")
	}
	request := releaseStartupFunction(t, "../validator/production_startup.go", "requestProductionPreparation")
	readyReturnsNil, pendingReturnsError := false, false
	ast.Inspect(request.Body, func(node ast.Node) bool {
		clause, ok := node.(*ast.CommClause)
		if !ok || len(clause.Body) == 0 {
			return true
		}
		returned, ok := clause.Body[len(clause.Body)-1].(*ast.ReturnStmt)
		if !ok || len(returned.Results) != 1 {
			return true
		}
		result := releaseStartupName(returned.Results[0])
		if receive, ok := clause.Comm.(*ast.ExprStmt); ok && releaseStartupName(receive.X) == "<-owner.ready" {
			readyReturnsNil = result == "nil"
		} else if clause.Comm == nil {
			pendingReturnsError = result != "" && result != "nil"
		}
		return true
	})
	if !readyReturnsNil || !pendingReturnsError {
		t.Fatal("a production intent can proceed before the preparation readiness")
	}
}

// One function from one production source, parsed with its own file set.
func releaseStartupFunction(t *testing.T, path, name string) *ast.FuncDecl {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, declaration := range parsed.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && function.Name.Name == name && function.Body != nil {
			return function
		}
	}
	t.Fatalf("production function %s is absent from %s", name, path)
	return nil
}

// Positions of every named call and every receive from a named channel.
func releaseStartupSteps(function *ast.FuncDecl) map[string][]token.Pos {
	steps := map[string][]token.Pos{}
	ast.Inspect(function.Body, func(node ast.Node) bool {
		switch value := node.(type) {
		case *ast.CallExpr:
			switch callee := value.Fun.(type) {
			case *ast.Ident:
				steps[callee.Name] = append(steps[callee.Name], value.Pos())
			case *ast.SelectorExpr:
				steps[callee.Sel.Name] = append(steps[callee.Sel.Name], value.Pos())
			}
		case *ast.UnaryExpr:
			if value.Op == token.ARROW {
				name := releaseStartupName(value)
				steps[name] = append(steps[name], value.Pos())
			}
		}
		return true
	})
	return steps
}

// A dotted identifier, nil, or a receive from one; anything else is empty.
func releaseStartupName(expression ast.Expr) string {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.SelectorExpr:
		if owner := releaseStartupName(value.X); owner != "" {
			return owner + "." + value.Sel.Name
		}
	case *ast.UnaryExpr:
		if owner := releaseStartupName(value.X); value.Op == token.ARROW && owner != "" {
			return "<-" + owner
		}
	}
	return ""
}
