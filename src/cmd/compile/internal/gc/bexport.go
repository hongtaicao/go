// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Binary package export.
// (see fmt.go, parser.go as "documentation" for how to use/setup data structures)
//
// Use "-newexport" flag to enable.

/*
Export data encoding:

The export data is a serialized description of the graph of exported
"objects": constants, types, variables, and functions. In general,
types - but also objects referred to from inlined function bodies -
can be reexported and so we need to know which package they are coming
from. Therefore, packages are also part of the export graph.

The roots of the graph are two lists of objects. The 1st list (phase 1,
see Export) contains all objects that are exported at the package level.
These objects are the full representation of the package's API, and they
are the only information a platform-independent tool (e.g., go/types)
needs to know to type-check against a package.

The 2nd list of objects contains all objects referred to from exported
inlined function bodies. These objects are needed by the compiler to
make sense of the function bodies; the exact list contents are compiler-
specific.

Finally, the export data contains a list of representations for inlined
function bodies. The format of this representation is compiler specific.

The graph is serialized in in-order fashion, starting with the roots.
Each object in the graph is serialized by writing its fields sequentially.
If the field is a pointer to another object, that object is serialized,
recursively. Otherwise the field is written. Non-pointer fields are all
encoded as integer or string values.

Only packages and types may be referred to more than once. When getting
to a package or type that was not serialized before, an integer _index_
is assigned to it, starting at 0. In this case, the encoding starts
with an integer _tag_ < 0. The tag value indicates the kind of object
(package or type) that follows and that this is the first time that we
see this object. If the package or tag was already serialized, the encoding
starts with the respective package or type index >= 0. An importer can
trivially determine if a package or type needs to be read in for the first
time (tag < 0) and entered into the respective package or type table, or
if the package or type was seen already (index >= 0), in which case the
index is used to look up the object in a table.

Before exporting or importing, the type tables are populated with the
predeclared types (int, string, error, unsafe.Pointer, etc.). This way
they are automatically encoded with a known and fixed type index.

TODO(gri) We may consider using the same sharing for other items
that are written out, such as strings, or possibly symbols (*Sym).

Encoding format:

The export data starts with a single byte indicating the encoding format
(compact, or with debugging information), followed by a version string
(so we can evolve the encoding if need be), the name of the imported
package, and a string containing platform-specific information for that
package.

After this header, two lists of objects and the list of inlined function
bodies follows.

The encoding of objects is straight-forward: Constants, variables, and
functions start with their name, type, and possibly a value. Named types
record their name and package so that they can be canonicalized: If the
same type was imported before via another import, the importer must use
the previously imported type pointer so that we have exactly one version
(i.e., one pointer) for each named type (and read but discard the current
type encoding). Unnamed types simply encode their respective fields.

In the encoding, some lists start with the list length (incl. strings).
Some lists are terminated with an end marker (usually for lists where
we may not know the length a priori).

All integer values use variable-length encoding for compact representation.

The exporter and importer are completely symmetric in implementation: For
each encoding routine there is a matching and symmetric decoding routine.
This symmetry makes it very easy to change or extend the format: If a new
field needs to be encoded, a symmetric change can be made to exporter and
importer.
*/

package gc

import (
	"bufio"
	"bytes"
	"cmd/compile/internal/big"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
)

// If debugFormat is set, each integer and string value is preceded by a marker
// and position information in the encoding. This mechanism permits an importer
// to recognize immediately when it is out of sync. The importer recognizes this
// mode automatically (i.e., it can import export data produced with debugging
// support even if debugFormat is not set at the time of import). This mode will
// lead to massively larger export data (by a factor of 2 to 3) and should only
// be enabled during development and debugging.
//
// NOTE: This flag is the first flag to enable if importing dies because of
// (suspected) format errors, and whenever a change is made to the format.
const debugFormat = false // default: false

// TODO(gri) remove eventually
const forceNewExport = false // force new export format - DO NOT SUBMIT with this flag set

const exportVersion = "v0"

// exportInlined enables the export of inlined function bodies and related
// dependencies. The compiler should work w/o any loss of functionality with
// the flag disabled, but the generated code will lose access to inlined
// function bodies across packages, leading to performance bugs.
// Leave for debugging.
const exportInlined = true // default: true

type exporter struct {
	out      *bufio.Writer
	pkgIndex map[*Pkg]int
	typIndex map[*Type]int
	inlined  []*Func

	// debugging support
	written int // bytes written
	indent  int // for p.trace
	trace   bool
}

// export writes the exportlist for localpkg to out and returns the number of bytes written.
func export(out *bufio.Writer, trace bool) int {
	p := exporter{
		out:      out,
		pkgIndex: make(map[*Pkg]int),
		typIndex: make(map[*Type]int),
		trace:    trace,
	}

	// first byte indicates low-level encoding format
	var format byte = 'c' // compact
	if debugFormat {
		format = 'd'
	}
	p.byte(format)

	// --- generic export data ---

	if p.trace {
		p.tracef("\n--- package ---\n")
		if p.indent != 0 {
			Fatalf("exporter: incorrect indentation %d", p.indent)
		}
	}

	if p.trace {
		p.tracef("version = ")
	}
	p.string(exportVersion)
	if p.trace {
		p.tracef("\n")
	}

	// populate type map with predeclared "known" types
	predecl := predeclared()
	for index, typ := range predecl {
		p.typIndex[typ] = index
	}
	if len(p.typIndex) != len(predecl) {
		Fatalf("exporter: duplicate entries in type map?")
	}

	// write package data
	if localpkg.Path != "" {
		Fatalf("exporter: local package path not empty: %q", localpkg.Path)
	}
	p.pkg(localpkg)
	if p.trace {
		p.tracef("\n")
	}

	// export objects
	//
	// First, export all exported (package-level) objects; i.e., all objects
	// in the current exportlist. These objects represent all information
	// required to import this package and type-check against it; i.e., this
	// is the platform-independent export data. The format is generic in the
	// sense that different compilers can use the same representation.
	//
	// During this first phase, more objects may be added to the exportlist
	// (due to inlined function bodies and their dependencies). Export those
	// objects in a second phase. That data is platform-specific as it depends
	// on the inlining decisions of the compiler and the representation of the
	// inlined function bodies.

	// remember initial exportlist length
	var numglobals = len(exportlist)

	// Phase 1: Export objects in _current_ exportlist; exported objects at
	//          package level.
	// Use range since we want to ignore objects added to exportlist during
	// this phase.
	objcount := 0
	for _, n := range exportlist {
		sym := n.Sym

		if sym.Flags&SymExported != 0 {
			continue
		}
		sym.Flags |= SymExported

		// TODO(gri) Closures have dots in their names;
		// e.g., TestFloatZeroValue.func1 in math/big tests.
		if strings.Contains(sym.Name, ".") {
			Fatalf("exporter: unexpected symbol: %v", sym)
		}

		// TODO(gri) Should we do this check?
		// if sym.Flags&SymExport == 0 {
		// 	continue
		// }

		if sym.Def == nil {
			Fatalf("exporter: unknown export symbol: %v", sym)
		}

		// TODO(gri) Optimization: Probably worthwhile collecting
		// long runs of constants and export them "in bulk" (saving
		// tags and types, and making import faster).

		if p.trace {
			p.tracef("\n")
		}
		p.obj(sym)
		objcount++
	}

	// indicate end of list
	if p.trace {
		p.tracef("\n")
	}
	p.tag(endTag)

	// for self-verification only (redundant)
	p.int(objcount)

	// --- compiler-specific export data ---

	if p.trace {
		p.tracef("\n--- compiler-specific export data ---\n[ ")
		if p.indent != 0 {
			Fatalf("exporter: incorrect indentation")
		}
	}

	// write compiler-specific flags
	p.bool(safemode != 0)
	if p.trace {
		p.tracef("\n")
	}

	// Phase 2: Export objects added to exportlist during phase 1.
	// Don't use range since exportlist may grow during this phase
	// and we want to export all remaining objects.
	objcount = 0
	for i := numglobals; exportInlined && i < len(exportlist); i++ {
		n := exportlist[i]
		sym := n.Sym

		// TODO(gri) The rest of this loop body is identical with
		// the loop body above. Leave alone for now since there
		// are different optimization opportunities, but factor
		// eventually.

		if sym.Flags&SymExported != 0 {
			continue
		}
		sym.Flags |= SymExported

		// TODO(gri) Closures have dots in their names;
		// e.g., TestFloatZeroValue.func1 in math/big tests.
		if strings.Contains(sym.Name, ".") {
			Fatalf("exporter: unexpected symbol: %v", sym)
		}

		// TODO(gri) Should we do this check?
		// if sym.Flags&SymExport == 0 {
		// 	continue
		// }

		if sym.Def == nil {
			Fatalf("exporter: unknown export symbol: %v", sym)
		}

		// TODO(gri) Optimization: Probably worthwhile collecting
		// long runs of constants and export them "in bulk" (saving
		// tags and types, and making import faster).

		if p.trace {
			p.tracef("\n")
		}
		p.obj(sym)
		objcount++
	}

	// indicate end of list
	if p.trace {
		p.tracef("\n")
	}
	p.tag(endTag)

	// for self-verification only (redundant)
	p.int(objcount)

	// --- inlined function bodies ---

	if p.trace {
		p.tracef("\n--- inlined function bodies ---\n[ ")
		if p.indent != 0 {
			Fatalf("exporter: incorrect indentation")
		}
	}

	// write inlined function bodies
	p.int(len(p.inlined))
	if p.trace {
		p.tracef("]\n")
	}
	for _, f := range p.inlined {
		if p.trace {
			p.tracef("\n----\nfunc { %s }\n", Hconv(f.Inl, FmtSharp))
		}
		p.stmtList(f.Inl)
		if p.trace {
			p.tracef("\n")
		}
	}

	if p.trace {
		p.tracef("\n--- end ---\n")
	}

	// --- end of export data ---

	return p.written
}

func (p *exporter) pkg(pkg *Pkg) {
	if pkg == nil {
		Fatalf("exporter: unexpected nil pkg")
	}

	// if we saw the package before, write its index (>= 0)
	if i, ok := p.pkgIndex[pkg]; ok {
		p.index('P', i)
		return
	}

	// otherwise, remember the package, write the package tag (< 0) and package data
	if p.trace {
		p.tracef("P%d = { ", len(p.pkgIndex))
		defer p.tracef("} ")
	}
	p.pkgIndex[pkg] = len(p.pkgIndex)

	p.tag(packageTag)
	p.string(pkg.Name)
	p.string(pkg.Path)
}

func unidealType(typ *Type, val Val) *Type {
	// Untyped (ideal) constants get their own type. This decouples
	// the constant type from the encoding of the constant value.
	if typ == nil || typ.IsUntyped() {
		typ = untype(val.Ctype())
	}
	return typ
}

func (p *exporter) obj(sym *Sym) {
	// Exported objects may be from different packages because they
	// may be re-exported as depencies when exporting inlined function
	// bodies. Thus, exported object names must be fully qualified.
	//
	// TODO(gri) This can only happen if exportInlined is enabled
	// (default), and during phase 2 of object export. Objects exported
	// in phase 1 (compiler-indendepent objects) are by definition only
	// the objects from the current package and not pulled in via inlined
	// function bodies. In that case the package qualifier is not needed.
	// Possible space optimization.

	n := sym.Def
	switch n.Op {
	case OLITERAL:
		// constant
		// TODO(gri) determine if we need the typecheck call here
		n = typecheck(n, Erv)
		if n == nil || n.Op != OLITERAL {
			Fatalf("exporter: dumpexportconst: oconst nil: %v", sym)
		}

		p.tag(constTag)
		// TODO(gri) In inlined functions, constants are used directly
		// so they should never occur as re-exported objects. We may
		// not need the qualified name here. See also comment above.
		// Possible space optimization.
		p.qualifiedName(sym)
		p.typ(unidealType(n.Type, n.Val()))
		p.value(n.Val())

	case OTYPE:
		// named type
		t := n.Type
		if t.Etype == TFORW {
			Fatalf("exporter: export of incomplete type %v", sym)
		}

		p.tag(typeTag)
		p.typ(t)

	case ONAME:
		// variable or function
		n = typecheck(n, Erv|Ecall)
		if n == nil || n.Type == nil {
			Fatalf("exporter: variable/function exported but not defined: %v", sym)
		}

		if n.Type.Etype == TFUNC && n.Class == PFUNC {
			// function
			p.tag(funcTag)
			p.qualifiedName(sym)

			sig := sym.Def.Type
			inlineable := isInlineable(sym.Def)

			p.paramList(sig.Params(), inlineable)
			p.paramList(sig.Results(), inlineable)

			index := -1
			if inlineable {
				index = len(p.inlined)
				p.inlined = append(p.inlined, sym.Def.Func)
				// TODO(gri) re-examine reexportdeplist:
				// Because we can trivially export types
				// in-place, we don't need to collect types
				// inside function bodies in the exportlist.
				// With an adjusted reexportdeplist used only
				// by the binary exporter, we can also avoid
				// the global exportlist.
				reexportdeplist(sym.Def.Func.Inl)
			}
			p.int(index)
		} else {
			// variable
			p.tag(varTag)
			p.qualifiedName(sym)
			p.typ(sym.Def.Type)
		}

	default:
		Fatalf("exporter: unexpected export symbol: %v %v", Oconv(n.Op, 0), sym)
	}
}

func isInlineable(n *Node) bool {
	if exportInlined && n != nil && n.Func != nil && len(n.Func.Inl.Slice()) != 0 {
		// when lazily typechecking inlined bodies, some re-exported ones may not have been typechecked yet.
		// currently that can leave unresolved ONONAMEs in import-dot-ed packages in the wrong package
		if Debug['l'] < 2 {
			typecheckinl(n)
		}
		return true
	}
	return false
}

func (p *exporter) typ(t *Type) {
	if t == nil {
		Fatalf("exporter: nil type")
	}

	// Possible optimization: Anonymous pointer types *T where
	// T is a named type are common. We could canonicalize all
	// such types *T to a single type PT = *T. This would lead
	// to at most one *T entry in typIndex, and all future *T's
	// would be encoded as the respective index directly. Would
	// save 1 byte (pointerTag) per *T and reduce the typIndex
	// size (at the cost of a canonicalization map). We can do
	// this later, without encoding format change.

	// if we saw the type before, write its index (>= 0)
	if i, ok := p.typIndex[t]; ok {
		p.index('T', i)
		return
	}

	// otherwise, remember the type, write the type tag (< 0) and type data
	if p.trace {
		p.tracef("T%d = {>\n", len(p.typIndex))
		defer p.tracef("<\n} ")
	}
	p.typIndex[t] = len(p.typIndex)

	// pick off named types
	if tsym := t.Sym; tsym != nil {
		// Predeclared types should have been found in the type map.
		if t.Orig == t {
			Fatalf("exporter: predeclared type missing from type map?")
		}
		// TODO(gri) The assertion below seems incorrect (crashes during all.bash).
		// we expect the respective definition to point to us
		// if tsym.Def.Type != t {
		// 	Fatalf("exporter: type definition doesn't point to us?")
		// }

		p.tag(namedTag)
		p.qualifiedName(tsym)

		// write underlying type
		p.typ(t.Orig)

		// interfaces don't have associated methods
		if t.Orig.IsInterface() {
			return
		}

		// sort methods for reproducible export format
		// TODO(gri) Determine if they are already sorted
		// in which case we can drop this step.
		var methods []*Field
		for _, m := range t.Methods().Slice() {
			methods = append(methods, m)
		}
		sort.Sort(methodbyname(methods))
		p.int(len(methods))

		if p.trace && len(methods) > 0 {
			p.tracef("associated methods {>")
		}

		for _, m := range methods {
			if p.trace {
				p.tracef("\n")
			}
			if strings.Contains(m.Sym.Name, ".") {
				Fatalf("invalid symbol name: %s (%v)", m.Sym.Name, m.Sym)
			}

			p.fieldSym(m.Sym, false)

			sig := m.Type
			mfn := sig.Nname()
			inlineable := isInlineable(mfn)

			p.paramList(sig.Recvs(), inlineable)
			p.paramList(sig.Params(), inlineable)
			p.paramList(sig.Results(), inlineable)

			index := -1
			if inlineable {
				index = len(p.inlined)
				p.inlined = append(p.inlined, mfn.Func)
				reexportdeplist(mfn.Func.Inl)
			}
			p.int(index)
		}

		if p.trace && len(methods) > 0 {
			p.tracef("<\n} ")
		}

		return
	}

	// otherwise we have a type literal
	switch t.Etype {
	case TARRAY:
		if t.isDDDArray() {
			Fatalf("array bounds should be known at export time: %v", t)
		}
		if t.IsArray() {
			p.tag(arrayTag)
			p.int64(t.NumElem())
		} else {
			p.tag(sliceTag)
		}
		p.typ(t.Elem())

	case TDDDFIELD:
		// see p.param use of TDDDFIELD
		p.tag(dddTag)
		p.typ(t.DDDField())

	case TSTRUCT:
		p.tag(structTag)
		p.fieldList(t)

	case TPTR32, TPTR64: // could use Tptr but these are constants
		p.tag(pointerTag)
		p.typ(t.Elem())

	case TFUNC:
		p.tag(signatureTag)
		p.paramList(t.Params(), false)
		p.paramList(t.Results(), false)

	case TINTER:
		p.tag(interfaceTag)

		// gc doesn't separate between embedded interfaces
		// and methods declared explicitly with an interface
		p.int(0) // no embedded interfaces
		p.methodList(t)

	case TMAP:
		p.tag(mapTag)
		p.typ(t.Key())
		p.typ(t.Val())

	case TCHAN:
		p.tag(chanTag)
		p.int(int(t.ChanDir()))
		p.typ(t.Elem())

	default:
		Fatalf("exporter: unexpected type: %s (Etype = %d)", Tconv(t, 0), t.Etype)
	}
}

func (p *exporter) qualifiedName(sym *Sym) {
	if strings.Contains(sym.Name, ".") {
		Fatalf("exporter: invalid symbol name: %s", sym.Name)
	}
	p.string(sym.Name)
	p.pkg(sym.Pkg)
}

func (p *exporter) fieldList(t *Type) {
	if p.trace && t.NumFields() > 0 {
		p.tracef("fields {>")
		defer p.tracef("<\n} ")
	}

	p.int(t.NumFields())
	for _, f := range t.Fields().Slice() {
		if p.trace {
			p.tracef("\n")
		}
		p.field(f)
	}
}

func (p *exporter) field(f *Field) {
	p.fieldName(f.Sym, f)
	p.typ(f.Type)
	p.note(f.Note)
}

func (p *exporter) note(n *string) {
	var s string
	if n != nil {
		s = *n
	}
	p.string(s)
}

func (p *exporter) methodList(t *Type) {
	if p.trace && t.NumFields() > 0 {
		p.tracef("methods {>")
		defer p.tracef("<\n} ")
	}

	p.int(t.NumFields())
	for _, m := range t.Fields().Slice() {
		if p.trace {
			p.tracef("\n")
		}
		p.method(m)
	}
}

func (p *exporter) method(m *Field) {
	p.fieldName(m.Sym, m)
	p.paramList(m.Type.Params(), false)
	p.paramList(m.Type.Results(), false)
}

// fieldName is like qualifiedName but it doesn't record the package
// for blank (_) or exported names.
func (p *exporter) fieldName(sym *Sym, t *Field) {
	if t != nil && sym != t.Sym {
		Fatalf("exporter: invalid fieldName parameters")
	}

	name := sym.Name
	if t != nil {
		if t.Embedded == 0 {
			name = sym.Name
		} else if bname := basetypeName(t.Type); bname != "" && !exportname(bname) {
			// anonymous field with unexported base type name: use "?" as field name
			// (bname != "" per spec, but we are conservative in case of errors)
			name = "?"
		} else {
			name = ""
		}
	}

	if strings.Contains(name, ".") {
		Fatalf("exporter: invalid symbol name: %s", name)
	}
	p.string(name)
	if name == "?" || name != "_" && name != "" && !exportname(name) {
		p.pkg(sym.Pkg)
	}
}

func basetypeName(t *Type) string {
	s := t.Sym
	if s == nil && t.IsPtr() {
		s = t.Elem().Sym // deref
	}
	if s != nil {
		if strings.Contains(s.Name, ".") {
			Fatalf("exporter: invalid symbol name: %s", s.Name)
		}
		return s.Name
	}
	return ""
}

func (p *exporter) paramList(params *Type, numbered bool) {
	if !params.IsFuncArgStruct() {
		Fatalf("exporter: parameter list expected")
	}

	// use negative length to indicate unnamed parameters
	// (look at the first parameter only since either all
	// names are present or all are absent)
	//
	// TODO(gri) If we don't have an exported function
	// body, the parameter names are irrelevant for the
	// compiler (though they may be of use for other tools).
	// Possible space optimization.
	n := params.NumFields()
	if n > 0 && parName(params.Field(0), numbered) == "" {
		n = -n
	}
	p.int(n)
	for _, q := range params.Fields().Slice() {
		p.param(q, n, numbered)
	}
}

func (p *exporter) param(q *Field, n int, numbered bool) {
	t := q.Type
	if q.Isddd {
		// create a fake type to encode ... just for the p.typ call
		t = typDDDField(t.Elem())
	}
	p.typ(t)
	if n > 0 {
		p.string(parName(q, numbered))
		// Because of (re-)exported inlined functions
		// the importpkg may not be the package to which this
		// function (and thus its parameter) belongs. We need to
		// supply the parameter package here. We need the package
		// when the function is inlined so we can properly resolve
		// the name.
		// TODO(gri) should do this only once per function/method
		p.pkg(q.Sym.Pkg)
	}
	// TODO(gri) This is compiler-specific (escape info).
	// Move into compiler-specific section eventually?
	// (Not having escape info causes tests to fail, e.g. runtime GCInfoTest)
	//
	// TODO(gri) The q.Note is much more verbose that necessary and
	// adds significantly to export data size. FIX THIS.
	p.note(q.Note)
}

func parName(f *Field, numbered bool) string {
	s := f.Sym
	if s == nil {
		return ""
	}

	// Take the name from the original, lest we substituted it with ~r%d or ~b%d.
	// ~r%d is a (formerly) unnamed result.
	if f.Nname != nil {
		if f.Nname.Orig != nil {
			s = f.Nname.Orig.Sym
			if s != nil && s.Name[0] == '~' {
				if s.Name[1] == 'r' { // originally an unnamed result
					return "" // s = nil
				} else if s.Name[1] == 'b' { // originally the blank identifier _
					return "_"
				}
			}
		} else {
			return "" // s = nil
		}
	}

	if s == nil {
		return ""
	}

	// print symbol with Vargen number or not as desired
	name := s.Name
	if strings.Contains(name, ".") {
		panic("invalid symbol name: " + name)
	}

	// Functions that can be inlined use numbered parameters so we can distingish them
	// from other names in their context after inlining (i.e., the parameter numbering
	// is a form of parameter rewriting). See issue 4326 for an example and test case.
	if numbered {
		if !strings.Contains(name, "·") && f.Nname != nil && f.Nname.Name != nil && f.Nname.Name.Vargen > 0 {
			name = fmt.Sprintf("%s·%d", name, f.Nname.Name.Vargen) // append Vargen
		}
	} else {
		if i := strings.Index(name, "·"); i > 0 {
			name = name[:i] // cut off Vargen
		}
	}
	return name
}

func (p *exporter) value(x Val) {
	if p.trace {
		p.tracef("= ")
	}

	switch x := x.U.(type) {
	case bool:
		tag := falseTag
		if x {
			tag = trueTag
		}
		p.tag(tag)

	case *Mpint:
		if Minintval[TINT64].Cmp(x) <= 0 && x.Cmp(Maxintval[TINT64]) <= 0 {
			// common case: x fits into an int64 - use compact encoding
			p.tag(int64Tag)
			p.int64(x.Int64())
			return
		}
		// uncommon case: large x - use float encoding
		// (powers of 2 will be encoded efficiently with exponent)
		f := newMpflt()
		f.SetInt(x)
		p.tag(floatTag)
		p.float(f)

	case *Mpflt:
		p.tag(floatTag)
		p.float(x)

	case *Mpcplx:
		p.tag(complexTag)
		p.float(&x.Real)
		p.float(&x.Imag)

	case string:
		p.tag(stringTag)
		p.string(x)

	case *NilVal:
		// not a constant but used in exported function bodies
		p.tag(nilTag)

	default:
		Fatalf("exporter: unexpected value %v (%T)", x, x)
	}
}

func (p *exporter) float(x *Mpflt) {
	// extract sign (there is no -0)
	f := &x.Val
	sign := f.Sign()
	if sign == 0 {
		// x == 0
		p.int(0)
		return
	}
	// x != 0

	// extract exponent such that 0.5 <= m < 1.0
	var m big.Float
	exp := f.MantExp(&m)

	// extract mantissa as *big.Int
	// - set exponent large enough so mant satisfies mant.IsInt()
	// - get *big.Int from mant
	m.SetMantExp(&m, int(m.MinPrec()))
	mant, acc := m.Int(nil)
	if acc != big.Exact {
		Fatalf("exporter: internal error")
	}

	p.int(sign)
	p.int(exp)
	p.string(string(mant.Bytes()))
}

// ----------------------------------------------------------------------------
// Inlined function bodies

// Approach: More or less closely follow what fmt.go is doing for FExp mode
// but instead of emitting the information textually, emit the node tree in
// binary form.

// stmtList may emit more (or fewer) than len(list) nodes.
func (p *exporter) stmtList(list Nodes) {
	if p.trace {
		if list.Len() == 0 {
			p.tracef("{}")
		} else {
			p.tracef("{>")
			defer p.tracef("<\n}")
		}
	}

	for _, n := range list.Slice() {
		if p.trace {
			p.tracef("\n")
		}
		// TODO inlining produces expressions with ninits. we can't export these yet.
		// (from fmt.go:1461ff)
		if opprec[n.Op] < 0 {
			p.stmt(n)
		} else {
			p.expr(n)
		}
	}

	p.op(OEND)
}

func (p *exporter) exprList(list Nodes) {
	if p.trace {
		if list.Len() == 0 {
			p.tracef("{}")
		} else {
			p.tracef("{>")
			defer p.tracef("<\n}")
		}
	}

	for _, n := range list.Slice() {
		if p.trace {
			p.tracef("\n")
		}
		p.expr(n)
	}

	p.op(OEND)
}

func (p *exporter) elemList(list Nodes) {
	if p.trace {
		p.tracef("[ ")
	}
	p.int(list.Len())
	if p.trace {
		if list.Len() == 0 {
			p.tracef("] {}")
		} else {
			p.tracef("] {>")
			defer p.tracef("<\n}")
		}
	}

	for _, n := range list.Slice() {
		if p.trace {
			p.tracef("\n")
		}
		p.fieldSym(n.Left.Sym, false)
		p.expr(n.Right)
	}
}

func (p *exporter) expr(n *Node) {
	if p.trace {
		p.tracef("( ")
		defer p.tracef(") ")
	}

	for n != nil && n.Implicit && (n.Op == OIND || n.Op == OADDR) {
		n = n.Left
	}

	switch op := n.Op; op {
	// expressions
	// (somewhat closely following the structure of exprfmt in fmt.go)
	case OPAREN:
		p.expr(n.Left) // unparen

	// case ODDDARG:
	//	unimplemented - handled by default case

	// case OREGISTER:
	//	unimplemented - handled by default case

	case OLITERAL:
		if n.Val().Ctype() == CTNIL && n.Orig != nil && n.Orig != n {
			p.expr(n.Orig)
			break
		}
		p.op(OLITERAL)
		p.typ(unidealType(n.Type, n.Val()))
		p.value(n.Val())

	case ONAME:
		// Special case: name used as local variable in export.
		// _ becomes ~b%d internally; print as _ for export
		if n.Sym != nil && n.Sym.Name[0] == '~' && n.Sym.Name[1] == 'b' {
			// case 0: mapped to ONAME
			p.op(ONAME)
			p.bool(true) // indicate blank identifier
			break
		}

		if n.Sym != nil && !isblank(n) && n.Name.Vargen > 0 {
			// case 1: mapped to OPACK
			p.op(OPACK)
			p.sym(n)
			break
		}

		// Special case: explicit name of func (*T) method(...) is turned into pkg.(*T).method,
		// but for export, this should be rendered as (*pkg.T).meth.
		// These nodes have the special property that they are names with a left OTYPE and a right ONAME.
		if n.Left != nil && n.Left.Op == OTYPE && n.Right != nil && n.Right.Op == ONAME {
			// case 2: mapped to ONAME
			p.op(ONAME)
			// TODO(gri) can we map this case directly to OXDOT
			//           and then get rid of the bool here?
			p.bool(false) // indicate non-blank identifier
			p.typ(n.Left.Type)
			p.fieldSym(n.Right.Sym, true)
			break
		}

		// case 3: mapped to OPACK
		p.op(OPACK)
		p.sym(n) // fallthrough inlined here

	case OPACK, ONONAME:
		p.op(op)
		p.sym(n)

	case OTYPE:
		p.op(OTYPE)
		if p.bool(n.Type == nil) {
			p.sym(n)
		} else {
			p.typ(n.Type)
		}

	case OTARRAY, OTMAP, OTCHAN, OTSTRUCT, OTINTER, OTFUNC:
		panic("unreachable") // should have been resolved by typechecking

	// case OCLOSURE:
	//	unimplemented - handled by default case

	// case OCOMPLIT:
	//	unimplemented - handled by default case

	case OPTRLIT:
		p.op(OPTRLIT)
		p.expr(n.Left)
		p.bool(n.Implicit)

	case OSTRUCTLIT:
		p.op(OSTRUCTLIT)
		if !p.bool(n.Implicit) {
			p.typ(n.Type)
		}
		p.elemList(n.List) // special handling of field names

	case OARRAYLIT, OMAPLIT:
		p.op(op)
		if !p.bool(n.Implicit) {
			p.typ(n.Type)
		}
		p.exprList(n.List)

	case OKEY:
		p.op(OKEY)
		p.exprsOrNil(n.Left, n.Right)

	// case OCALLPART:
	//	unimplemented - handled by default case

	case OXDOT, ODOT, ODOTPTR, ODOTINTER, ODOTMETH:
		p.op(OXDOT)
		p.expr(n.Left)
		if n.Sym == nil {
			panic("unreachable") // can this happen during export?
		}
		p.fieldSym(n.Sym, true)

	case ODOTTYPE, ODOTTYPE2:
		p.op(ODOTTYPE)
		p.expr(n.Left)
		if p.bool(n.Right != nil) {
			p.expr(n.Right)
		} else {
			p.typ(n.Type)
		}

	case OINDEX, OINDEXMAP:
		p.op(OINDEX)
		p.expr(n.Left)
		p.expr(n.Right)

	case OSLICE, OSLICESTR, OSLICEARR:
		p.op(OSLICE)
		p.expr(n.Left)
		p.expr(n.Right)

	case OSLICE3, OSLICE3ARR:
		p.op(OSLICE3)
		p.expr(n.Left)
		p.expr(n.Right)

	case OCOPY, OCOMPLEX:
		p.op(op)
		p.expr(n.Left)
		p.expr(n.Right)

	case OCONV, OCONVIFACE, OCONVNOP, OARRAYBYTESTR, OARRAYRUNESTR, OSTRARRAYBYTE, OSTRARRAYRUNE, ORUNESTR:
		p.op(OCONV)
		p.typ(n.Type)
		if p.bool(n.Left != nil) {
			p.expr(n.Left)
		} else {
			p.exprList(n.List)
		}

	case OREAL, OIMAG, OAPPEND, OCAP, OCLOSE, ODELETE, OLEN, OMAKE, ONEW, OPANIC, ORECOVER, OPRINT, OPRINTN:
		p.op(op)
		if p.bool(n.Left != nil) {
			p.expr(n.Left)
		} else {
			p.exprList(n.List)
			p.bool(n.Isddd)
		}

	case OCALL, OCALLFUNC, OCALLMETH, OCALLINTER, OGETG:
		p.op(OCALL)
		p.expr(n.Left)
		p.exprList(n.List)
		p.bool(n.Isddd)

	case OMAKEMAP, OMAKECHAN, OMAKESLICE:
		p.op(op) // must keep separate from OMAKE for importer
		p.typ(n.Type)
		switch {
		default:
			// empty list
			p.op(OEND)
		case n.List.Len() != 0: // pre-typecheck
			p.exprList(n.List) // emits terminating OEND
		case n.Right != nil:
			p.expr(n.Left)
			p.expr(n.Right)
			p.op(OEND)
		case n.Left != nil && (n.Op == OMAKESLICE || !n.Left.Type.IsUntyped()):
			p.expr(n.Left)
			p.op(OEND)
		}

	// unary expressions
	case OPLUS, OMINUS, OADDR, OCOM, OIND, ONOT, ORECV:
		p.op(op)
		p.expr(n.Left)

	// binary expressions
	case OADD, OAND, OANDAND, OANDNOT, ODIV, OEQ, OGE, OGT, OLE, OLT,
		OLSH, OMOD, OMUL, ONE, OOR, OOROR, ORSH, OSEND, OSUB, OXOR:
		p.op(op)
		p.expr(n.Left)
		p.expr(n.Right)

	case OADDSTR:
		p.op(OADDSTR)
		p.exprList(n.List)

	case OCMPSTR, OCMPIFACE:
		p.op(Op(n.Etype))
		p.expr(n.Left)
		p.expr(n.Right)

	case ODCLCONST:
		// if exporting, DCLCONST should just be removed as its usage
		// has already been replaced with literals
		// TODO(gri) these should not be exported in the first place
		// TODO(gri) why is this considered an expression in fmt.go?
		p.op(ODCLCONST)

	default:
		Fatalf("exporter: CANNOT EXPORT: %s\nPlease notify gri@\n", opnames[n.Op])
	}
}

// Caution: stmt will emit more than one node for statement nodes n that have a non-empty
// n.Ninit and where n cannot have a natural init section (such as in "if", "for", etc.).
func (p *exporter) stmt(n *Node) {
	if p.trace {
		p.tracef("( ")
		defer p.tracef(") ")
	}

	if n.Ninit.Len() > 0 && !stmtwithinit(n.Op) {
		if p.trace {
			p.tracef("( /* Ninits */ ")
		}

		// can't use stmtList here since we don't want the final OEND
		for _, n := range n.Ninit.Slice() {
			p.stmt(n)
		}

		if p.trace {
			p.tracef(") ")
		}
	}

	switch op := n.Op; op {
	case ODCL:
		p.op(ODCL)
		switch n.Left.Class &^ PHEAP {
		case PPARAM, PPARAMOUT, PAUTO:
			// TODO(gri) when is this not PAUTO?
			// Also, originally this didn't look like
			// the default case. Investigate.
			fallthrough
		default:
			// TODO(gri) Can we ever reach here?
			p.bool(false)
			p.sym(n.Left)
		}
		p.typ(n.Left.Type)

	// case ODCLFIELD:
	//	unimplemented - handled by default case

	case OAS, OASWB:
		p.op(op)
		// Don't export "v = <N>" initializing statements, hope they're always
		// preceded by the DCL which will be re-parsed and typecheck to reproduce
		// the "v = <N>" again.
		// TODO(gri) if n.Right == nil, don't emit anything
		if p.bool(n.Right != nil) {
			p.expr(n.Left)
			p.expr(n.Right)
		}

	case OASOP:
		p.op(OASOP)
		p.int(int(n.Etype))
		p.expr(n.Left)
		if p.bool(!n.Implicit) {
			p.expr(n.Right)
		}

	case OAS2:
		p.op(OAS2)
		p.exprList(n.List)
		p.exprList(n.Rlist)

	case OAS2DOTTYPE, OAS2FUNC, OAS2MAPR, OAS2RECV:
		p.op(op)
		p.exprList(n.List)
		p.exprList(n.Rlist)

	case ORETURN:
		p.op(ORETURN)
		p.exprList(n.List)

	case ORETJMP:
		// generated by compiler for trampolin routines - not exported
		panic("unreachable")

	case OPROC, ODEFER:
		p.op(op)
		p.expr(n.Left)

	case OIF:
		p.op(OIF)
		p.stmtList(n.Ninit)
		p.expr(n.Left)
		p.stmtList(n.Nbody)
		p.stmtList(n.Rlist)

	case OFOR:
		p.op(OFOR)
		p.stmtList(n.Ninit)
		p.exprsOrNil(n.Left, n.Right)
		p.stmtList(n.Nbody)

	case ORANGE:
		p.op(ORANGE)
		p.stmtList(n.List)
		p.expr(n.Right)
		p.stmtList(n.Nbody)

	case OSELECT, OSWITCH:
		p.op(op)
		p.stmtList(n.Ninit)
		p.exprsOrNil(n.Left, nil)
		p.stmtList(n.List)

	case OCASE, OXCASE:
		p.op(op)
		p.stmtList(n.List)
		p.stmtList(n.Nbody)

	case OBREAK, OCONTINUE, OGOTO, OFALL, OXFALL:
		p.op(op)
		p.exprsOrNil(n.Left, nil)

	case OEMPTY:
	// nothing to emit

	case OLABEL:
		p.op(OLABEL)
		p.expr(n.Left)

	default:
		Fatalf("exporter: CANNOT EXPORT: %s\nPlease notify gri@\n", opnames[n.Op])
	}
}

func (p *exporter) exprsOrNil(a, b *Node) {
	ab := 0
	if a != nil {
		ab |= 1
	}
	if b != nil {
		ab |= 2
	}
	p.int(ab)
	if ab&1 != 0 {
		p.expr(a)
	}
	if ab&2 != 0 {
		p.expr(b)
	}
}

func (p *exporter) fieldSym(s *Sym, short bool) {
	name := s.Name

	// remove leading "type." in method names ("(T).m" -> "m")
	if short {
		if i := strings.LastIndex(name, "."); i >= 0 {
			name = name[i+1:]
		}
	}

	p.string(name)
	if !exportname(name) {
		p.pkg(s.Pkg)
	}
}

func (p *exporter) sym(n *Node) {
	s := n.Sym
	if s.Pkg != nil {
		if len(s.Name) > 0 && s.Name[0] == '.' {
			Fatalf("exporter: exporting synthetic symbol %s", s.Name)
		}
	}

	if p.trace {
		p.tracef("{ SYM ")
		defer p.tracef("} ")
	}

	name := s.Name

	// remove leading "type." in method names ("(T).m" -> "m")
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}

	if strings.Contains(name, "·") && n.Name.Vargen > 0 {
		Fatalf("exporter: unexpected · in symbol name")
	}

	if i := n.Name.Vargen; i > 0 {
		name = fmt.Sprintf("%s·%d", name, i)
	}

	p.string(name)
	if name != "_" {
		p.pkg(s.Pkg)
	}
}

func (p *exporter) bool(b bool) bool {
	if p.trace {
		p.tracef("[")
		defer p.tracef("= %v] ", b)
	}

	x := 0
	if b {
		x = 1
	}
	p.int(x)
	return b
}

func (p *exporter) op(op Op) {
	if p.trace {
		p.tracef("[")
		defer p.tracef("= %s] ", opnames[op])
	}

	p.int(int(op))
}

// ----------------------------------------------------------------------------
// Low-level encoders

func (p *exporter) index(marker byte, index int) {
	if index < 0 {
		Fatalf("exporter: invalid index < 0")
	}
	if debugFormat {
		p.marker('t')
	}
	if p.trace {
		p.tracef("%c%d ", marker, index)
	}
	p.rawInt64(int64(index))
}

func (p *exporter) tag(tag int) {
	if tag >= 0 {
		Fatalf("exporter: invalid tag >= 0")
	}
	if debugFormat {
		p.marker('t')
	}
	if p.trace {
		p.tracef("%s ", tagString[-tag])
	}
	p.rawInt64(int64(tag))
}

func (p *exporter) int(x int) {
	p.int64(int64(x))
}

func (p *exporter) int64(x int64) {
	if debugFormat {
		p.marker('i')
	}
	if p.trace {
		p.tracef("%d ", x)
	}
	p.rawInt64(x)
}

func (p *exporter) string(s string) {
	if debugFormat {
		p.marker('s')
	}
	if p.trace {
		p.tracef("%q ", s)
	}
	p.rawInt64(int64(len(s)))
	for i := 0; i < len(s); i++ {
		p.byte(s[i])
	}
}

// marker emits a marker byte and position information which makes
// it easy for a reader to detect if it is "out of sync". Used only
// if debugFormat is set.
func (p *exporter) marker(m byte) {
	p.byte(m)
	// Uncomment this for help tracking down the location
	// of an incorrect marker when running in debugFormat.
	// if p.trace {
	// 	p.tracef("#%d ", p.written)
	// }
	p.rawInt64(int64(p.written))
}

// rawInt64 should only be used by low-level encoders
func (p *exporter) rawInt64(x int64) {
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutVarint(tmp[:], x)
	for i := 0; i < n; i++ {
		p.byte(tmp[i])
	}
}

// byte is the bottleneck interface to write to p.out.
// byte escapes b as follows (any encoding does that
// hides '$'):
//
//	'$'  => '|' 'S'
//	'|'  => '|' '|'
//
// Necessary so other tools can find the end of the
// export data by searching for "$$".
func (p *exporter) byte(b byte) {
	switch b {
	case '$':
		// write '$' as '|' 'S'
		b = 'S'
		fallthrough
	case '|':
		// write '|' as '|' '|'
		p.out.WriteByte('|')
		p.written++
	}
	p.out.WriteByte(b)
	p.written++
}

// tracef is like fmt.Printf but it rewrites the format string
// to take care of indentation.
func (p *exporter) tracef(format string, args ...interface{}) {
	if strings.ContainsAny(format, "<>\n") {
		var buf bytes.Buffer
		for i := 0; i < len(format); i++ {
			// no need to deal with runes
			ch := format[i]
			switch ch {
			case '>':
				p.indent++
				continue
			case '<':
				p.indent--
				continue
			}
			buf.WriteByte(ch)
			if ch == '\n' {
				for j := p.indent; j > 0; j-- {
					buf.WriteString(".  ")
				}
			}
		}
		format = buf.String()
	}
	fmt.Printf(format, args...)
}

// ----------------------------------------------------------------------------
// Export format

// Tags. Must be < 0.
const (
	// Objects
	packageTag = -(iota + 1)
	constTag
	typeTag
	varTag
	funcTag
	endTag

	// Types
	namedTag
	arrayTag
	sliceTag
	dddTag
	structTag
	pointerTag
	signatureTag
	interfaceTag
	mapTag
	chanTag

	// Values
	falseTag
	trueTag
	int64Tag
	floatTag
	fractionTag // not used by gc
	complexTag
	stringTag
	nilTag
	unknownTag // not used by gc (only appears in packages with errors)
)

// Debugging support.
// (tagString is only used when tracing is enabled)
var tagString = [...]string{
	// Objects
	-packageTag: "package",
	-constTag:   "const",
	-typeTag:    "type",
	-varTag:     "var",
	-funcTag:    "func",
	-endTag:     "end",

	// Types
	-namedTag:     "named type",
	-arrayTag:     "array",
	-sliceTag:     "slice",
	-dddTag:       "ddd",
	-structTag:    "struct",
	-pointerTag:   "pointer",
	-signatureTag: "signature",
	-interfaceTag: "interface",
	-mapTag:       "map",
	-chanTag:      "chan",

	// Values
	-falseTag:    "false",
	-trueTag:     "true",
	-int64Tag:    "int64",
	-floatTag:    "float",
	-fractionTag: "fraction",
	-complexTag:  "complex",
	-stringTag:   "string",
	-nilTag:      "nil",
	-unknownTag:  "unknown",
}

// untype returns the "pseudo" untyped type for a Ctype (import/export use only).
// (we can't use an pre-initialized array because we must be sure all types are
// set up)
func untype(ctype Ctype) *Type {
	switch ctype {
	case CTINT:
		return idealint
	case CTRUNE:
		return idealrune
	case CTFLT:
		return idealfloat
	case CTCPLX:
		return idealcomplex
	case CTSTR:
		return idealstring
	case CTBOOL:
		return idealbool
	case CTNIL:
		return Types[TNIL]
	}
	Fatalf("exporter: unknown Ctype")
	return nil
}

var predecl []*Type // initialized lazily

func predeclared() []*Type {
	if predecl == nil {
		// initialize lazily to be sure that all
		// elements have been initialized before
		predecl = []*Type{
			// basic types
			Types[TBOOL],
			Types[TINT],
			Types[TINT8],
			Types[TINT16],
			Types[TINT32],
			Types[TINT64],
			Types[TUINT],
			Types[TUINT8],
			Types[TUINT16],
			Types[TUINT32],
			Types[TUINT64],
			Types[TUINTPTR],
			Types[TFLOAT32],
			Types[TFLOAT64],
			Types[TCOMPLEX64],
			Types[TCOMPLEX128],
			Types[TSTRING],

			// aliases
			bytetype,
			runetype,

			// error
			errortype,

			// untyped types
			untype(CTBOOL),
			untype(CTINT),
			untype(CTRUNE),
			untype(CTFLT),
			untype(CTCPLX),
			untype(CTSTR),
			untype(CTNIL),

			// package unsafe
			Types[TUNSAFEPTR],

			// invalid type (package contains errors)
			Types[Txxx],

			// any type, for builtin export data
			Types[TANY],
		}
	}
	return predecl
}
