package op_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"golang.org/x/exp/slog"

	"github.com/ethereum/go-ethereum/log"

	"github.com/ethereum-optimism/optimism/op-service/testlog"
)

// Plan is the default entry-point to use for op-test tests.
// It wraps the Go test framework to provide test utils and parametrization features.
func Plan(t *testing.T, fn func(t Planner)) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var selector ParameterSelector
	ctx = context.WithValue(ctx, parameterManagerCtxKey{}, selector)

	imp := &testImpl{
		T:      t,
		ctx:    ctx,
		logLvl: slog.LevelError,
	}
	imp.Plan("default", fn)
	imp.exhaust(fn)
}

type parameterCtxKey string

type parameterSelection struct {
	name    string
	options []string
}

// testImpl wraps the regular Go test framework to implement the Testing interface.
type testImpl struct {
	*testing.T

	// ctx is scoped to the execution of this test-scope.
	// ctx contains all parametrization choices made thus far.
	// ctx is updated with default-choices the test may make along the way.
	ctx context.Context
	// we substitute the context when selecting parameters/values
	ctxLock sync.RWMutex

	logLvl slog.Level

	loggerOnce sync.Once
	logger     log.Logger

	// First-seen parameterSelection, which can be exhausted at the end of the test.
	parameterSelection *parameterSelection
}

var _ Planner = (*testImpl)(nil)

// Ctx implements Testing.Ctx
func (imp *testImpl) Ctx() context.Context {
	imp.ctxLock.RLock()
	v := imp.ctx
	imp.ctxLock.RUnlock()
	return v
}

// Logger implements Testing.Logger
func (imp *testImpl) Logger() log.Logger {
	imp.loggerOnce.Do(func() {
		imp.logger = testlog.Logger(imp, imp.logLvl)
	})
	return imp.logger
}

// Parameter implements Testing.Parameter
func (imp *testImpl) Parameter(name string) (value string, ok bool) {
	v := imp.Ctx().Value(parameterCtxKey(name))
	if v == nil {
		return "", false
	}
	return v.(string), true
}

// Run implements Planner.Run
func (imp *testImpl) Run(name string, fn func(t Executor)) {
	// TODO check if in immediate (execute now) or deferred (persist test-plan) mode

	ctx := imp.Ctx()

	// immediate
	imp.T.Run(name, func(t *testing.T) {
		ctx, cancel := context.WithCancel(ctx)
		t.Cleanup(cancel)

		subScope := &testImpl{
			T:      t,
			ctx:    ctx,
			logLvl: imp.logLvl,
		}
		fn(subScope)
	})
}

// Plan implements Planner.Plan
func (imp *testImpl) Plan(name string, fn func(t Planner)) {
	imp.planCtx(imp.Ctx(), name, fn)
}

// planCtx runs a sub-test with a custom context
func (imp *testImpl) planCtx(ctx context.Context, name string, fn func(t Planner)) {
	imp.T.Run(name, func(t *testing.T) {
		ctx, cancel := context.WithCancel(ctx)
		t.Cleanup(cancel)

		subScope := &testImpl{
			T:      t,
			ctx:    ctx,
			logLvl: imp.logLvl,
		}
		fn(subScope)

		// after completing the default path, try to exhaust the parameters we discovered and have not already made
		subScope.exhaust(fn)
	})
}

// exhaust reviews if any options were seen in the current test-scope, and then exhausts these.
func (imp *testImpl) exhaust(fn func(t Planner)) {
	if imp.parameterSelection == nil { // no parameters to exhaust
		return
	}

	ctx := imp.Ctx()
	for _, opt := range imp.parameterSelection.options {
		key := parameterCtxKey(imp.parameterSelection.name)

		// If choice already matches the context, then we already made it in the default path.
		current := ctx.Value(key)
		if current == nil {
			imp.T.Fatalf("test framework error: selecting %q, "+
				"but exhaust-path is not running after default path", imp.parameterSelection.name)
		}
		if current.(string) == opt {
			continue
		}

		// Run a sub-test that overrides the default choice we may have made (if any).
		subCtx := context.WithValue(ctx, key, opt)
		imp.planCtx(subCtx, "exhaust_"+imp.parameterSelection.name+"_"+opt, fn)
	}
}

// selected registers that a set of options was available for a named parameter,
// and registers the first option as chosen.
// It is invalid to signal an empty set of selected options.
// It is invalid to signal selected options for a parameter that was already selected.
func (imp *testImpl) selected(name string, options ...string) {
	if len(options) == 0 {
		imp.T.Fatalf("cannot signal empty set of options of type %q", name)
	}
	current := imp.ctx.Value(parameterCtxKey(name))
	if current != nil {
		imp.T.Fatalf("test signaled options of type %q, but an option already selected: %q",
			name, current.(string))
	}
	imp.parameterSelection = &parameterSelection{name: name, options: options}
	imp.ctx = context.WithValue(imp.ctx, parameterCtxKey(name), options[0])
}

// Select implements Testing.Select
func (imp *testImpl) Select(name string, options ...string) string {
	// Check if the choice was already made
	imp.ctxLock.Lock()
	defer imp.ctxLock.Unlock()
	current := imp.ctx.Value(parameterCtxKey(name))
	hasWildcard := slices.Contains(options, "*")
	if current != nil {
		if !hasWildcard && !slices.Contains(options, current.(string)) {
			imp.T.Fatalf("presented with choice %q, with options %q, but already assumed %q",
				name, strings.Join(options, ", "), current.(string))
		}
		return current.(string)
	}

	// get the parameter selector
	selector := imp.ctx.Value(parameterManagerCtxKey{}).(ParameterSelector)
	// select what option(s) we should go with
	selectedOptions := selector.Select(name, options)
	if len(selectedOptions) == 0 {
		imp.T.Skipf("None of the options for parameter %q where selected, skipping test!", name)
	}
	if !hasWildcard {
		// verify the selected options are valid (a subset of the suggested options)
		seen := make(map[string]struct{})
		for _, opt := range options {
			seen[opt] = struct{}{}
		}
		for _, opt := range selectedOptions {
			if _, ok := seen[opt]; !ok {
				imp.T.Fatalf("Test selector selected option %q for %q, but it is was not in the set of selectable options!", opt, name)
			}
		}
	}
	// register what options we selected
	imp.selected(name, selectedOptions...)
	// return the option we went with as default
	return selectedOptions[0]
}