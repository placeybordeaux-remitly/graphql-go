package exec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/placeybordeaux-remitly/graphql-go/errors"
	"github.com/placeybordeaux-remitly/graphql-go/internal/common"
	"github.com/placeybordeaux-remitly/graphql-go/internal/exec/resolvable"
	"github.com/placeybordeaux-remitly/graphql-go/internal/exec/selected"
	"github.com/placeybordeaux-remitly/graphql-go/internal/query"
	"github.com/placeybordeaux-remitly/graphql-go/internal/schema"
	"github.com/placeybordeaux-remitly/graphql-go/log"
	"github.com/placeybordeaux-remitly/graphql-go/trace"
)

type Request struct {
	selected.Request
	Limiter                  chan struct{}
	Tracer                   trace.Tracer
	Logger                   log.Logger
	SubscribeResolverTimeout time.Duration
}

func (r *Request) handlePanic(ctx context.Context) {
	if value := recover(); value != nil {
		r.Logger.LogPanic(ctx, value)
		r.AddError(makePanicError(value))
	}
}

type extensionser interface {
	Extensions() map[string]interface{}
}

func makePanicError(value interface{}) *errors.QueryError {
	return errors.Errorf("panic occurred: %v", value)
}

func (r *Request) Execute(ctx context.Context, s *resolvable.Schema, op *query.Operation) ([]byte, []*errors.QueryError) {
	var out bytes.Buffer
	func() {
		defer r.handlePanic(ctx)
		sels := selected.ApplyOperation(&r.Request, s, op)
		r.execSelections(ctx, sels, nil, s, s.Resolver, &out, op.Type == query.Mutation)
	}()

	if err := ctx.Err(); err != nil {
		//If context has either been cancelled or timed out we still may want to return the features that have finished
		// TODO properly attribute mark which features have timedout in the error field
		return out.Bytes(), r.Errs
	}

	return out.Bytes(), r.Errs
}

type fieldToExec struct {
	field    *selected.SchemaField
	sels     []selected.Selection
	resolver reflect.Value
	out      *bytes.Buffer
	lock     sync.Mutex
	finished bool
}

func resolvedToNull(b *bytes.Buffer) bool {
	return bytes.Equal(b.Bytes(), []byte("null"))
}

func (r *Request) execSelections(ctx context.Context, sels []selected.Selection, path *pathSegment, s *resolvable.Schema, resolver reflect.Value, out *bytes.Buffer, serially bool) {
	async := !serially && selected.HasAsyncSel(sels)

	var fields []*fieldToExec
	collectFieldsToResolve(sels, s, resolver, &fields, make(map[string]*fieldToExec))

	if async {
		var wg sync.WaitGroup
		wg.Add(len(fields))
		for _, f := range fields {
			go func(f *fieldToExec) {
				defer wg.Done()
				defer r.handlePanic(ctx)
				f.out = new(bytes.Buffer)
				execFieldSelection(ctx, r, s, f, &pathSegment{path, f.field.Alias}, true)
			}(f)
		}
		// close a signal channel once the wait group is complete
		done := make(chan struct{})
		go func() {
			defer close(done)
			wg.Wait()
		}()
		// We want to block until either the context is done or the wait group is complete
		select {
		case <-done:
		case <-ctx.Done():
		}
	} else {
		for _, f := range fields {
			f.out = new(bytes.Buffer)
			execFieldSelection(ctx, r, s, f, &pathSegment{path, f.field.Alias}, true)
		}
	}

	out.WriteByte('{')
	for i, f := range fields {
		f.lock.Lock()
		// If a non-nullable child resolved to null, an error was added to the
		// "errors" list in the response, so this field resolves to null.
		// If this field is non-nullable, the error is propagated to its parent.
		if _, ok := f.field.Type.(*common.NonNull); ok && (resolvedToNull(f.out) || !f.finished) {
			out.Reset()
			out.Write([]byte("null"))
			if !f.finished { // if we haven't finished yet that means we haven't recorded this failure yet
				err := errors.Errorf(ctx.Err().Error())
				err.Path = append(path.toSlice(), f.field.Alias)
				r.AddError(err)
			}
			f.lock.Unlock()
			return
		}

		if i > 0 {
			out.WriteByte(',')
		}
		out.WriteByte('"')
		out.WriteString(f.field.Alias)
		out.WriteByte('"')
		out.WriteByte(':')
		// if this field hasn't finished yet, then it's timed out. Record it as null
		if !f.finished {
			err := errors.Errorf(ctx.Err().Error())
			err.Path = append(path.toSlice(), f.field.Alias)
			r.AddError(err)
			out.WriteString("null")
			continue
		}
		out.Write(f.out.Bytes())
		f.lock.Unlock()
	}
	out.WriteByte('}')
}

func collectFieldsToResolve(sels []selected.Selection, s *resolvable.Schema, resolver reflect.Value, fields *[]*fieldToExec, fieldByAlias map[string]*fieldToExec) {
	for _, sel := range sels {
		switch sel := sel.(type) {
		case *selected.SchemaField:
			field, ok := fieldByAlias[sel.Alias]
			if !ok { // validation already checked for conflict (TODO)
				field = &fieldToExec{field: sel, resolver: resolver}
				fieldByAlias[sel.Alias] = field
				*fields = append(*fields, field)
			}
			field.sels = append(field.sels, sel.Sels...)

		case *selected.TypenameField:
			sf := &selected.SchemaField{
				Field:       s.Meta.FieldTypename,
				Alias:       sel.Alias,
				FixedResult: reflect.ValueOf(typeOf(sel, resolver)),
			}
			*fields = append(*fields, &fieldToExec{field: sf, resolver: resolver})

		case *selected.TypeAssertion:
			out := resolver.Method(sel.MethodIndex).Call(nil)
			if !out[1].Bool() {
				continue
			}
			collectFieldsToResolve(sel.Sels, s, out[0], fields, fieldByAlias)

		default:
			panic("unreachable")
		}
	}
}

func typeOf(tf *selected.TypenameField, resolver reflect.Value) string {
	if len(tf.TypeAssertions) == 0 {
		return tf.Name
	}
	for name, a := range tf.TypeAssertions {
		out := resolver.Method(a.MethodIndex).Call(nil)
		if out[1].Bool() {
			return name
		}
	}
	return ""
}

func execFieldSelection(ctx context.Context, r *Request, s *resolvable.Schema, f *fieldToExec, path *pathSegment, applyLimiter bool) {
	if applyLimiter {
		r.Limiter <- struct{}{}
	}

	var result reflect.Value
	var err *errors.QueryError

	traceCtx, finish := r.Tracer.TraceField(ctx, f.field.TraceLabel, f.field.TypeName, f.field.Name, !f.field.Async, f.field.Args)
	defer func() {
		finish(err)
	}()

	err = func() (err *errors.QueryError) {
		defer func() {
			f.lock.Lock()
			f.finished = true
			if panicValue := recover(); panicValue != nil {
				r.Logger.LogPanic(ctx, panicValue)
				err = makePanicError(panicValue)
				err.Path = path.toSlice()
			}
			f.lock.Unlock()
		}()

		if f.field.FixedResult.IsValid() {
			result = f.field.FixedResult
			return nil
		}

		if err := traceCtx.Err(); err != nil {
			return errors.Errorf("%s", err) // don't execute any more resolvers if context got cancelled
		}

		res := f.resolver
		if f.field.UseMethodResolver() {
			var in []reflect.Value
			if f.field.HasContext {
				in = append(in, reflect.ValueOf(traceCtx))
			}
			if f.field.ArgsPacker != nil {
				in = append(in, f.field.PackedArgs)
			}
			callOut := res.Method(f.field.MethodIndex).Call(in)
			result = callOut[0]
			if f.field.HasError && !callOut[1].IsNil() {
				resolverErr := callOut[1].Interface().(error)
				err := errors.Errorf("%s", resolverErr)
				err.Path = path.toSlice()
				err.ResolverError = resolverErr
				if ex, ok := callOut[1].Interface().(extensionser); ok {
					err.Extensions = ex.Extensions()
				}
				return err
			}
		} else {
			// TODO extract out unwrapping ptr logic to a common place
			if res.Kind() == reflect.Ptr {
				res = res.Elem()
			}
			result = res.FieldByIndex(f.field.FieldIndex)
		}
		return nil
	}()

	if applyLimiter {
		<-r.Limiter
	}

	if err != nil {
		// If an error occurred while resolving a field, it should be treated as though the field
		// returned null, and an error must be added to the "errors" list in the response.
		r.AddError(err)
		f.lock.Lock()
		f.out.WriteString("null")
		f.lock.Unlock()
		return
	}

	f.lock.Lock()
	r.execSelectionSet(traceCtx, f.sels, f.field.Type, path, s, result, f.out)
	f.lock.Unlock()
}

func (r *Request) execSelectionSet(ctx context.Context, sels []selected.Selection, typ common.Type, path *pathSegment, s *resolvable.Schema, resolver reflect.Value, out *bytes.Buffer) {
	t, nonNull := unwrapNonNull(typ)

	// a reflect.Value of a nil interface will show up as an Invalid value
	if resolver.Kind() == reflect.Invalid || ((resolver.Kind() == reflect.Ptr || resolver.Kind() == reflect.Interface) && resolver.IsNil()) {
		// If a field of a non-null type resolves to null (either because the
		// function to resolve the field returned null or because an error occurred),
		// add an error to the "errors" list in the response.
		if nonNull {
			err := errors.Errorf("graphql: got nil for non-null %q", t)
			err.Path = path.toSlice()
			r.AddError(err)
		}
		out.WriteString("null")
		return
	}

	switch t.(type) {
	case *schema.Object, *schema.Interface, *schema.Union:
		r.execSelections(ctx, sels, path, s, resolver, out, false)
		return
	}

	// Any pointers or interfaces at this point should be non-nil, so we can get the actual value of them
	// for serialization
	if resolver.Kind() == reflect.Ptr || resolver.Kind() == reflect.Interface {
		resolver = resolver.Elem()
	}

	switch t := t.(type) {
	case *common.List:
		r.execList(ctx, sels, t, path, s, resolver, out)

	case *schema.Scalar:
		v := resolver.Interface()
		data, err := json.Marshal(v)
		if err != nil {
			panic(errors.Errorf("could not marshal %v: %s", v, err))
		}
		out.Write(data)

	case *schema.Enum:
		var stringer fmt.Stringer = resolver
		if s, ok := resolver.Interface().(fmt.Stringer); ok {
			stringer = s
		}
		name := stringer.String()
		var valid bool
		for _, v := range t.Values {
			if v.Name == name {
				valid = true
				break
			}
		}
		if !valid {
			err := errors.Errorf("Invalid value %s.\nExpected type %s, found %s.", name, t.Name, name)
			err.Path = path.toSlice()
			r.AddError(err)
			out.WriteString("null")
			return
		}
		out.WriteByte('"')
		out.WriteString(name)
		out.WriteByte('"')

	default:
		panic("unreachable")
	}
}

func (r *Request) execList(ctx context.Context, sels []selected.Selection, typ *common.List, path *pathSegment, s *resolvable.Schema, resolver reflect.Value, out *bytes.Buffer) {
	l := resolver.Len()
	entryouts := make([]bytes.Buffer, l)

	if selected.HasAsyncSel(sels) {
		// Limit the number of concurrent goroutines spawned as it can lead to large
		// memory spikes for large lists.
		concurrency := cap(r.Limiter)
		sem := make(chan struct{}, concurrency)
		for i := 0; i < l; i++ {
			sem <- struct{}{}
			go func(i int) {
				defer func() { <-sem }()
				defer r.handlePanic(ctx)
				r.execSelectionSet(ctx, sels, typ.OfType, &pathSegment{path, i}, s, resolver.Index(i), &entryouts[i])
			}(i)
		}
		for i := 0; i < concurrency; i++ {
			sem <- struct{}{}
		}
	} else {
		for i := 0; i < l; i++ {
			r.execSelectionSet(ctx, sels, typ.OfType, &pathSegment{path, i}, s, resolver.Index(i), &entryouts[i])
		}
	}

	_, listOfNonNull := typ.OfType.(*common.NonNull)

	out.WriteByte('[')
	for i, entryout := range entryouts {
		// If the list wraps a non-null type and one of the list elements
		// resolves to null, then the entire list resolves to null.
		if listOfNonNull && resolvedToNull(&entryout) {
			out.Reset()
			out.WriteString("null")
			return
		}

		if i > 0 {
			out.WriteByte(',')
		}
		out.Write(entryout.Bytes())
	}
	out.WriteByte(']')
}

func unwrapNonNull(t common.Type) (common.Type, bool) {
	if nn, ok := t.(*common.NonNull); ok {
		return nn.OfType, true
	}
	return t, false
}

type pathSegment struct {
	parent *pathSegment
	value  interface{}
}

func (p *pathSegment) toSlice() []interface{} {
	if p == nil {
		return nil
	}
	return append(p.parent.toSlice(), p.value)
}
