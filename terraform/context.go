package terraform

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/hashicorp/terraform/config"
	"github.com/hashicorp/terraform/depgraph"
	"github.com/hashicorp/terraform/helper/multierror"
)

// This is a function type used to implement a walker for the resource
// tree internally on the Terraform structure.
type genericWalkFunc func(*Resource) (map[string]string, error)

// Context represents all the context that Terraform needs in order to
// perform operations on infrastructure. This structure is built using
// ContextOpts and NewContext. See the documentation for those.
//
// Additionally, a context can be created from a Plan using Plan.Context.
type Context struct {
	config    *config.Config
	diff      *Diff
	hooks     []Hook
	state     *State
	providers map[string]ResourceProviderFactory
	variables map[string]string

	l     sync.Mutex
	runCh <-chan struct{}
	sh    *stopHook
}

// ContextOpts are the user-creatable configuration structure to create
// a context with NewContext.
type ContextOpts struct {
	Config    *config.Config
	Diff      *Diff
	Hooks     []Hook
	State     *State
	Providers map[string]ResourceProviderFactory
	Variables map[string]string
}

// NewContext creates a new context.
//
// Once a context is created, the pointer values within ContextOpts should
// not be mutated in any way, since the pointers are copied, not the values
// themselves.
func NewContext(opts *ContextOpts) *Context {
	sh := new(stopHook)

	// Copy all the hooks and add our stop hook. We don't append directly
	// to the Config so that we're not modifying that in-place.
	hooks := make([]Hook, len(opts.Hooks)+1)
	copy(hooks, opts.Hooks)
	hooks[len(opts.Hooks)] = sh

	return &Context{
		config:    opts.Config,
		diff:      opts.Diff,
		hooks:     hooks,
		state:     opts.State,
		providers: opts.Providers,
		variables: opts.Variables,

		sh: sh,
	}
}

// Apply applies the changes represented by this context and returns
// the resulting state.
//
// In addition to returning the resulting state, this context is updated
// with the latest state.
func (c *Context) Apply() (*State, error) {
	v := c.acquireRun()
	defer c.releaseRun(v)

	g, err := Graph(&GraphOpts{
		Config:    c.config,
		Diff:      c.diff,
		Providers: c.providers,
		State:     c.state,
	})
	if err != nil {
		return nil, err
	}

	// Create our result. Make sure we preserve the prior states
	s := new(State)
	s.init()
	if c.state != nil {
		for k, v := range c.state.Resources {
			s.Resources[k] = v
		}
	}

	// Walk
	err = g.Walk(c.applyWalkFn(s))

	// Update our state, even if we have an error, for partial updates
	c.state = s

	// If we have no errors, then calculate the outputs if we have any
	if err == nil && len(c.config.Outputs) > 0 {
		s.Outputs = make(map[string]string)
		for _, o := range c.config.Outputs {
			if err = c.computeVars(s, o.RawConfig); err != nil {
				break
			}

			s.Outputs[o.Name] = o.RawConfig.Config()["value"].(string)
		}
	}

	return s, err
}

// Plan generates an execution plan for the given context.
//
// The execution plan encapsulates the context and can be stored
// in order to reinstantiate a context later for Apply.
//
// Plan also updates the diff of this context to be the diff generated
// by the plan, so Apply can be called after.
func (c *Context) Plan(opts *PlanOpts) (*Plan, error) {
	v := c.acquireRun()
	defer c.releaseRun(v)

	g, err := Graph(&GraphOpts{
		Config:    c.config,
		Providers: c.providers,
		State:     c.state,
	})
	if err != nil {
		return nil, err
	}

	p := &Plan{
		Config: c.config,
		Vars:   c.variables,
		State:  c.state,
	}
	err = g.Walk(c.planWalkFn(p, opts))

	// Update the diff so that our context is up-to-date
	c.diff = p.Diff

	return p, err
}

// Refresh goes through all the resources in the state and refreshes them
// to their latest state. This will update the state that this context
// works with, along with returning it.
//
// Even in the case an error is returned, the state will be returned and
// will potentially be partially updated.
func (c *Context) Refresh() (*State, error) {
	v := c.acquireRun()
	defer c.releaseRun(v)

	g, err := Graph(&GraphOpts{
		Config:    c.config,
		Providers: c.providers,
		State:     c.state,
	})
	if err != nil {
		return c.state, err
	}

	s := new(State)
	s.init()
	err = g.Walk(c.refreshWalkFn(s))

	// Update our state
	c.state = s

	return s, err
}

// Stop stops the running task.
//
// Stop will block until the task completes.
func (c *Context) Stop() {
	c.l.Lock()
	ch := c.runCh

	// If we aren't running, then just return
	if ch == nil {
		c.l.Unlock()
		return
	}

	// Tell the hook we want to stop
	c.sh.Stop()

	// Wait for us to stop
	c.l.Unlock()
	<-ch
}

// Validate validates the configuration and returns any warnings or errors.
func (c *Context) Validate() ([]string, []error) {
	var rerr *multierror.Error

	// Validate the configuration itself
	if err := c.config.Validate(); err != nil {
		rerr = multierror.ErrorAppend(rerr, err)
	}

	// Validate the user variables
	if errs := smcUserVariables(c.config, c.variables); len(errs) > 0 {
		rerr = multierror.ErrorAppend(rerr, errs...)
	}

	// Validate the graph
	g, err := c.graph()
	if err != nil {
		rerr = multierror.ErrorAppend(rerr, fmt.Errorf(
			"Error creating graph: %s", err))
	}

	// Walk the graph and validate all the configs
	var warns []string
	var errs []error
	err = g.Walk(c.validateWalkFn(&warns, &errs))
	if err != nil {
		rerr = multierror.ErrorAppend(rerr, fmt.Errorf(
			"Error validating resources in graph: %s", err))
	}
	if len(errs) > 0 {
		rerr = multierror.ErrorAppend(rerr, errs...)
	}

	errs = nil
	if rerr != nil && len(rerr.Errors) > 0 {
		errs = rerr.Errors
	}

	return warns, errs
}

// computeVars takes the State and given RawConfig and processes all
// the variables. This dynamically discovers the attributes instead of
// using a static map[string]string that the genericWalkFn uses.
func (c *Context) computeVars(s *State, raw *config.RawConfig) error {
	// If there are on variables, then we're done
	if len(raw.Variables) == 0 {
		return nil
	}

	// Go through each variable and find it
	vs := make(map[string]string)
	for n, rawV := range raw.Variables {
		switch v := rawV.(type) {
		case *config.ResourceVariable:
			r, ok := s.Resources[v.ResourceId()]
			if !ok {
				return fmt.Errorf(
					"Resource '%s' not found for variable '%s'",
					v.ResourceId(),
					v.FullKey())
			}

			attr, ok := r.Attributes[v.Field]
			if !ok {
				return fmt.Errorf(
					"Resource '%s' does not have attribute '%s' "+
						"for variable '%s'",
					v.ResourceId(),
					v.Field,
					v.FullKey())
			}

			vs[n] = attr
		case *config.UserVariable:
			vs[n] = c.variables[v.Name]
		}
	}

	// Interpolate the variables
	return raw.Interpolate(vs)
}

func (c *Context) graph() (*depgraph.Graph, error) {
	return Graph(&GraphOpts{
		Config:    c.config,
		Diff:      c.diff,
		Providers: c.providers,
		State:     c.state,
	})
}

func (c *Context) acquireRun() chan<- struct{} {
	c.l.Lock()
	defer c.l.Unlock()

	// Wait for no channel to exist
	for c.runCh != nil {
		c.l.Unlock()
		ch := c.runCh
		<-ch
		c.l.Lock()
	}

	ch := make(chan struct{})
	c.runCh = ch
	return ch
}

func (c *Context) releaseRun(ch chan<- struct{}) {
	c.l.Lock()
	defer c.l.Unlock()

	close(ch)
	c.runCh = nil
	c.sh.Reset()
}

func (c *Context) applyWalkFn(result *State) depgraph.WalkFunc {
	var l sync.Mutex

	// Initialize the result
	result.init()

	cb := func(r *Resource) (map[string]string, error) {
		diff := r.Diff
		if diff.Empty() {
			return r.Vars(), nil
		}

		if !diff.Destroy {
			var err error
			diff, err = r.Provider.Diff(r.State, r.Config)
			if err != nil {
				return nil, err
			}
		}

		// TODO(mitchellh): we need to verify the diff doesn't change
		// anything and that the diff has no computed values (pre-computed)

		for _, h := range c.hooks {
			handleHook(h.PreApply(r.Id, r.State, diff))
		}

		// With the completed diff, apply!
		log.Printf("[DEBUG] %s: Executing Apply", r.Id)
		rs, err := r.Provider.Apply(r.State, diff)
		if err != nil {
			return nil, err
		}

		// Make sure the result is instantiated
		if rs == nil {
			rs = new(ResourceState)
		}

		// Force the resource state type to be our type
		rs.Type = r.State.Type

		var errs []error
		for ak, av := range rs.Attributes {
			// If the value is the unknown variable value, then it is an error.
			// In this case we record the error and remove it from the state
			if av == config.UnknownVariableValue {
				errs = append(errs, fmt.Errorf(
					"Attribute with unknown value: %s", ak))
				delete(rs.Attributes, ak)
			}
		}

		// Update the resulting diff
		l.Lock()
		if rs.ID == "" {
			delete(result.Resources, r.Id)
		} else {
			result.Resources[r.Id] = rs
		}
		l.Unlock()

		// Update the state for the resource itself
		r.State = rs

		for _, h := range c.hooks {
			handleHook(h.PostApply(r.Id, r.State))
		}

		// Determine the new state and update variables
		err = nil
		if len(errs) > 0 {
			err = &multierror.Error{Errors: errs}
		}

		return r.Vars(), err
	}

	return c.genericWalkFn(c.variables, cb)
}

func (c *Context) planWalkFn(result *Plan, opts *PlanOpts) depgraph.WalkFunc {
	var l sync.Mutex

	// If we were given nil options, instantiate it
	if opts == nil {
		opts = new(PlanOpts)
	}

	// Initialize the result
	result.init()

	cb := func(r *Resource) (map[string]string, error) {
		var diff *ResourceDiff

		for _, h := range c.hooks {
			handleHook(h.PreDiff(r.Id, r.State))
		}

		if opts.Destroy {
			if r.State.ID != "" {
				log.Printf("[DEBUG] %s: Making for destroy", r.Id)
				diff = &ResourceDiff{Destroy: true}
			} else {
				log.Printf("[DEBUG] %s: Not marking for destroy, no ID", r.Id)
			}
		} else if r.Config == nil {
			log.Printf("[DEBUG] %s: Orphan, marking for destroy", r.Id)

			// This is an orphan (no config), so we mark it to be destroyed
			diff = &ResourceDiff{Destroy: true}
		} else {
			log.Printf("[DEBUG] %s: Executing diff", r.Id)

			// Get a diff from the newest state
			var err error
			diff, err = r.Provider.Diff(r.State, r.Config)
			if err != nil {
				return nil, err
			}
		}

		l.Lock()
		if !diff.Empty() {
			result.Diff.Resources[r.Id] = diff
		}
		l.Unlock()

		for _, h := range c.hooks {
			handleHook(h.PostDiff(r.Id, diff))
		}

		// Determine the new state and update variables
		if !diff.Empty() {
			r.State = r.State.MergeDiff(diff)
		}

		return r.Vars(), nil
	}

	return c.genericWalkFn(c.variables, cb)
}

func (c *Context) refreshWalkFn(result *State) depgraph.WalkFunc {
	var l sync.Mutex

	cb := func(r *Resource) (map[string]string, error) {
		for _, h := range c.hooks {
			handleHook(h.PreRefresh(r.Id, r.State))
		}

		rs, err := r.Provider.Refresh(r.State)
		if err != nil {
			return nil, err
		}
		if rs == nil {
			rs = new(ResourceState)
		}

		// Fix the type to be the type we have
		rs.Type = r.State.Type

		l.Lock()
		result.Resources[r.Id] = rs
		l.Unlock()

		for _, h := range c.hooks {
			handleHook(h.PostRefresh(r.Id, rs))
		}

		return nil, nil
	}

	return c.genericWalkFn(c.variables, cb)
}

func (c *Context) validateWalkFn(rws *[]string, res *[]error) depgraph.WalkFunc {
	return func(n *depgraph.Noun) error {
		// If it is the root node, ignore
		if n.Name == GraphRootNode {
			return nil
		}

		switch rn := n.Meta.(type) {
		case *GraphNodeResource:
			if rn.Resource == nil {
				panic("resource should never be nil")
			}

			// If it doesn't have a provider, that is a different problem
			if rn.Resource.Provider == nil {
				return nil
			}

			log.Printf("[INFO] Validating resource: %s", rn.Resource.Id)
			ws, es := rn.Resource.Provider.ValidateResource(
				rn.Type, rn.Resource.Config)
			for i, w := range ws {
				ws[i] = fmt.Sprintf("'%s' warning: %s", rn.Resource.Id, w)
			}
			for i, e := range es {
				es[i] = fmt.Errorf("'%s' error: %s", rn.Resource.Id, e)
			}

			*rws = append(*rws, ws...)
			*res = append(*res, es...)
		case *GraphNodeResourceProvider:
			if rn.Config == nil {
				return nil
			}

			rc := NewResourceConfig(rn.Config.RawConfig)

			for k, p := range rn.Providers {
				log.Printf("[INFO] Validating provider: %s", k)
				ws, es := p.Validate(rc)
				for i, w := range ws {
					ws[i] = fmt.Sprintf("Provider '%s' warning: %s", k, w)
				}
				for i, e := range es {
					es[i] = fmt.Errorf("Provider '%s' error: %s", k, e)
				}

				*rws = append(*rws, ws...)
				*res = append(*res, es...)
			}
		}

		return nil
	}
}

func (c *Context) genericWalkFn(
	invars map[string]string,
	cb genericWalkFunc) depgraph.WalkFunc {
	var l sync.RWMutex

	// Initialize the variables for application
	vars := make(map[string]string)
	for k, v := range invars {
		vars[fmt.Sprintf("var.%s", k)] = v
	}

	// This will keep track of the counts of multi-count resources
	counts := make(map[string]int)

	// This will keep track of whether we're stopped or not
	var stop uint32 = 0

	return func(n *depgraph.Noun) error {
		// If it is the root node, ignore
		if n.Name == GraphRootNode {
			return nil
		}

		// If we're stopped, return right away
		if atomic.LoadUint32(&stop) != 0 {
			return nil
		}

		// Calculate any aggregate interpolated variables if we have to.
		// Aggregate variables (such as "test_instance.foo.*.id") are not
		// pre-computed since the fanout would be expensive. We calculate
		// them on-demand here.
		computeAggregateVars(&l, n, counts, vars)

		switch m := n.Meta.(type) {
		case *GraphNodeResource:
		case *GraphNodeResourceMeta:
			// Record the count and then just ignore
			l.Lock()
			counts[m.ID] = m.Count
			l.Unlock()
			return nil
		case *GraphNodeResourceProvider:
			var rc *ResourceConfig
			if m.Config != nil {
				if err := m.Config.RawConfig.Interpolate(vars); err != nil {
					panic(err)
				}
				rc = NewResourceConfig(m.Config.RawConfig)
			}

			for k, p := range m.Providers {
				log.Printf("[INFO] Configuring provider: %s", k)
				err := p.Configure(rc)
				if err != nil {
					return err
				}
			}

			return nil
		default:
			panic(fmt.Sprintf("unknown graph node: %#v", n.Meta))
		}

		rn := n.Meta.(*GraphNodeResource)

		l.RLock()
		if len(vars) > 0 && rn.Config != nil {
			if err := rn.Config.RawConfig.Interpolate(vars); err != nil {
				panic(fmt.Sprintf("Interpolate error: %s", err))
			}

			// Force the config to be set later
			rn.Resource.Config = nil
		}
		l.RUnlock()

		// Make sure that at least some resource configuration is set
		if !rn.Orphan {
			if rn.Resource.Config == nil {
				if rn.Config == nil {
					rn.Resource.Config = new(ResourceConfig)
				} else {
					rn.Resource.Config = NewResourceConfig(rn.Config.RawConfig)
				}
			}
		} else {
			rn.Resource.Config = nil
		}

		// Handle recovery of special panic scenarios
		defer func() {
			if v := recover(); v != nil {
				if v == HookActionHalt {
					atomic.StoreUint32(&stop, 1)
				} else {
					panic(v)
				}
			}
		}()

		// Call the callack
		log.Printf("[INFO] Walking: %s", rn.Resource.Id)
		newVars, err := cb(rn.Resource)
		if err != nil {
			return err
		}

		if len(newVars) > 0 {
			// Acquire a lock since this function is called in parallel
			l.Lock()
			defer l.Unlock()

			// Update variables
			for k, v := range newVars {
				vars[k] = v
			}
		}

		return nil
	}
}

func computeAggregateVars(
	l *sync.RWMutex,
	n *depgraph.Noun,
	cs map[string]int,
	vs map[string]string) {
	var ivars map[string]config.InterpolatedVariable
	switch m := n.Meta.(type) {
	case *GraphNodeResource:
		if m.Config != nil {
			ivars = m.Config.RawConfig.Variables
		}
	case *GraphNodeResourceProvider:
		if m.Config != nil {
			ivars = m.Config.RawConfig.Variables
		}
	}
	if len(ivars) == 0 {
		return
	}

	for _, v := range ivars {
		rv, ok := v.(*config.ResourceVariable)
		if !ok {
			continue
		}

		idx := strings.Index(rv.Field, ".")
		if idx == -1 {
			// It isn't an aggregated var
			continue
		}
		if rv.Field[:idx] != "*" {
			// It isn't an aggregated var
			continue
		}
		field := rv.Field[idx+1:]

		// Get the meta node so that we can determine the count
		key := fmt.Sprintf("%s.%s", rv.Type, rv.Name)
		l.RLock()
		count, ok := cs[key]
		l.RUnlock()
		if !ok {
			// This should never happen due to semantic checks
			panic(fmt.Sprintf(
				"non-existent resource variable access: %s\n\n%#v", key, rv))
		}

		var values []string
		for i := 0; i < count; i++ {
			key := fmt.Sprintf(
				"%s.%s.%d.%s",
				rv.Type,
				rv.Name,
				i,
				field)
			if v, ok := vs[key]; ok {
				values = append(values, v)
			}
		}

		vs[rv.FullKey()] = strings.Join(values, ",")
	}
}
