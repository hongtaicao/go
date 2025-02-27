// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Binary package import.
// Based loosely on x/tools/go/importer.

package gc

import (
	"bufio"
	"cmd/compile/internal/big"
	"encoding/binary"
	"fmt"
)

// The overall structure of Import is symmetric to Export: For each
// export method in bexport.go there is a matching and symmetric method
// in bimport.go. Changing the export format requires making symmetric
// changes to bimport.go and bexport.go.

type importer struct {
	in       *bufio.Reader
	buf      []byte   // for reading strings
	bufarray [64]byte // initial underlying array for buf, large enough to avoid allocation when compiling std lib
	pkgList  []*Pkg
	typList  []*Type
	inlined  []*Node // functions with pending inlined function bodies

	// debugging support
	debugFormat bool
	read        int // bytes read
}

// Import populates importpkg from the serialized package data.
func Import(in *bufio.Reader) {
	p := importer{in: in}
	p.buf = p.bufarray[:]

	// read low-level encoding format
	switch format := p.byte(); format {
	case 'c':
		// compact format - nothing to do
	case 'd':
		p.debugFormat = true
	default:
		Fatalf("importer: invalid encoding format in export data: got %q; want 'c' or 'd'", format)
	}

	// --- generic export data ---

	if v := p.string(); v != exportVersion {
		Fatalf("importer: unknown export data version: %s", v)
	}

	// populate typList with predeclared "known" types
	p.typList = append(p.typList, predeclared()...)

	// read package data
	p.pkg()
	if p.pkgList[0] != importpkg {
		Fatalf("importer: imported package not found in pkgList[0]")
	}

	// defer some type-checking until all types are read in completely
	// (parser.go:import_package)
	tcok := typecheckok
	typecheckok = true
	defercheckwidth()

	// read objects

	// phase 1
	objcount := 0
	for {
		tag := p.tagOrIndex()
		if tag == endTag {
			break
		}
		p.obj(tag)
		objcount++
	}

	// self-verification
	if count := p.int(); count != objcount {
		Fatalf("importer: got %d objects; want %d", objcount, count)
	}

	// --- compiler-specific export data ---

	// read compiler-specific flags
	importpkg.Safe = p.bool()

	// phase 2
	objcount = 0
	for {
		tag := p.tagOrIndex()
		if tag == endTag {
			break
		}
		p.obj(tag)
		objcount++
	}

	// self-verification
	if count := p.int(); count != objcount {
		Fatalf("importer: got %d objects; want %d", objcount, count)
	}

	// read inlined functions bodies
	if dclcontext != PEXTERN {
		Fatalf("importer: unexpected context %d", dclcontext)
	}

	bcount := p.int() // consistency check only
	if bcount != len(p.inlined) {
		Fatalf("importer: expected %d inlined function bodies; got %d", bcount, len(p.inlined))
	}
	for _, f := range p.inlined {
		if Funcdepth != 0 {
			Fatalf("importer: unexpected Funcdepth %d", Funcdepth)
		}
		if f != nil {
			// function body not yet imported - read body and set it
			funchdr(f)
			f.Func.Inl.Set(p.stmtList())
			funcbody(f)
		} else {
			// function already imported - read body but discard declarations
			dclcontext = PDISCARD // throw away any declarations
			p.stmtList()
			dclcontext = PEXTERN
		}
	}

	if dclcontext != PEXTERN {
		Fatalf("importer: unexpected context %d", dclcontext)
	}

	// --- end of export data ---

	typecheckok = tcok
	resumecheckwidth()

	testdclstack() // debugging only
}

func (p *importer) pkg() *Pkg {
	// if the package was seen before, i is its index (>= 0)
	i := p.tagOrIndex()
	if i >= 0 {
		return p.pkgList[i]
	}

	// otherwise, i is the package tag (< 0)
	if i != packageTag {
		Fatalf("importer: expected package tag, found tag = %d", i)
	}

	// read package data
	name := p.string()
	path := p.string()

	// we should never see an empty package name
	if name == "" {
		Fatalf("importer: empty package name in import")
	}

	// we should never see a bad import path
	if isbadimport(path) {
		Fatalf("importer: bad path in import: %q", path)
	}

	// an empty path denotes the package we are currently importing
	pkg := importpkg
	if path != "" {
		pkg = mkpkg(path)
	}
	if pkg.Name == "" {
		pkg.Name = name
	} else if pkg.Name != name {
		Fatalf("importer: conflicting names %s and %s for package %q", pkg.Name, name, path)
	}
	p.pkgList = append(p.pkgList, pkg)

	return pkg
}

func idealType(typ *Type) *Type {
	if typ.IsUntyped() {
		// canonicalize ideal types
		typ = Types[TIDEAL]
	}
	return typ
}

func (p *importer) obj(tag int) {
	switch tag {
	case constTag:
		sym := p.qualifiedName()
		typ := p.typ()
		val := p.value(typ)
		importconst(sym, idealType(typ), nodlit(val))

	case typeTag:
		p.typ()

	case varTag:
		sym := p.qualifiedName()
		typ := p.typ()
		importvar(sym, typ)

	case funcTag:
		sym := p.qualifiedName()
		params := p.paramList()
		result := p.paramList()
		inl := p.int()

		sig := functype(nil, params, result)
		importsym(sym, ONAME)
		if sym.Def != nil && sym.Def.Op == ONAME {
			if Eqtype(sig, sym.Def.Type) {
				// function was imported before (via another import)
				dclcontext = PDISCARD // since we skip funchdr below
			} else {
				Fatalf("importer: inconsistent definition for func %v during import\n\t%v\n\t%v", sym, sym.Def.Type, sig)
			}
		}

		var n *Node
		if dclcontext != PDISCARD {
			n = newfuncname(sym)
			n.Type = sig
			declare(n, PFUNC)
			if inl < 0 {
				funchdr(n)
			}
		}

		if inl >= 0 {
			// function has inlined body - collect for later
			if inl != len(p.inlined) {
				Fatalf("importer: inlined index = %d; want %d", inl, len(p.inlined))
			}
			p.inlined = append(p.inlined, n)
		}

		// parser.go:hidden_import
		if dclcontext == PDISCARD {
			dclcontext = PEXTERN // since we skip the funcbody below
			break
		}

		if inl < 0 {
			funcbody(n)
		}
		importlist = append(importlist, n) // TODO(gri) may only be needed for inlineable functions

		if Debug['E'] > 0 {
			fmt.Printf("import [%q] func %v \n", importpkg.Path, n)
			if Debug['m'] > 2 && len(n.Func.Inl.Slice()) != 0 {
				fmt.Printf("inl body: %v\n", n.Func.Inl)
			}
		}

	default:
		Fatalf("importer: unexpected object (tag = %d)", tag)
	}
}

func (p *importer) newtyp(etype EType) *Type {
	t := typ(etype)
	p.typList = append(p.typList, t)
	return t
}

func (p *importer) typ() *Type {
	// if the type was seen before, i is its index (>= 0)
	i := p.tagOrIndex()
	if i >= 0 {
		return p.typList[i]
	}

	// otherwise, i is the type tag (< 0)
	var t *Type
	switch i {
	case namedTag:
		// parser.go:hidden_importsym
		tsym := p.qualifiedName()

		// parser.go:hidden_pkgtype
		t = pkgtype(tsym)
		p.typList = append(p.typList, t)

		// read underlying type
		// parser.go:hidden_type
		t0 := p.typ()
		importtype(t, t0) // parser.go:hidden_import

		// interfaces don't have associated methods
		if t0.IsInterface() {
			break
		}

		// set correct import context (since p.typ() may be called
		// while importing the body of an inlined function)
		savedContext := dclcontext
		dclcontext = PEXTERN

		// read associated methods
		for i := p.int(); i > 0; i-- {
			// parser.go:hidden_fndcl

			sym := p.fieldSym()

			recv := p.paramList() // TODO(gri) do we need a full param list for the receiver?
			params := p.paramList()
			result := p.paramList()
			inl := p.int()

			n := methodname1(newname(sym), recv[0].Right)
			n.Type = functype(recv[0], params, result)
			checkwidth(n.Type)
			addmethod(sym, n.Type, tsym.Pkg, false, false)
			if inl < 0 {
				funchdr(n)
			}

			if inl >= 0 {
				// method has inlined body - collect for later
				if inl != len(p.inlined) {
					Fatalf("importer: inlined index = %d; want %d", inl, len(p.inlined))
				}
				p.inlined = append(p.inlined, n)
			}

			// (comment from parser.go)
			// inl.C's inlnode in on a dotmeth node expects to find the inlineable body as
			// (dotmeth's type).Nname.Inl, and dotmeth's type has been pulled
			// out by typecheck's lookdot as this $$.ttype. So by providing
			// this back link here we avoid special casing there.
			n.Type.SetNname(n)

			// parser.go:hidden_import
			if inl < 0 {
				funcbody(n)
			}
			importlist = append(importlist, n) // TODO(gri) may only be needed for inlineable functions

			if Debug['E'] > 0 {
				fmt.Printf("import [%q] meth %v \n", importpkg.Path, n)
				if Debug['m'] > 2 && len(n.Func.Inl.Slice()) != 0 {
					fmt.Printf("inl body: %v\n", n.Func.Inl)
				}
			}
		}

		dclcontext = savedContext

	case arrayTag, sliceTag:
		t = p.newtyp(TARRAY)
		var bound int64
		if i == arrayTag {
			bound = p.int64()
		}
		elem := p.typ()
		if i == arrayTag {
			t.Extra = &ArrayType{Elem: elem, Bound: bound}
		} else {
			t.Extra = SliceType{Elem: elem}
		}

	case dddTag:
		t = p.newtyp(TDDDFIELD)
		t.Extra = DDDFieldType{T: p.typ()}

	case structTag:
		t = p.newtyp(TSTRUCT)
		tostruct0(t, p.fieldList())

	case pointerTag:
		t = p.newtyp(Tptr)
		t.Extra = PtrType{Elem: p.typ()}

	case signatureTag:
		t = p.newtyp(TFUNC)
		params := p.paramList()
		result := p.paramList()
		functype0(t, nil, params, result)

	case interfaceTag:
		t = p.newtyp(TINTER)
		if p.int() != 0 {
			Fatalf("importer: unexpected embedded interface")
		}
		tointerface0(t, p.methodList())

	case mapTag:
		t = p.newtyp(TMAP)
		mt := t.MapType()
		mt.Key = p.typ()
		mt.Val = p.typ()

	case chanTag:
		t = p.newtyp(TCHAN)
		ct := t.ChanType()
		ct.Dir = ChanDir(p.int())
		ct.Elem = p.typ()

	default:
		Fatalf("importer: unexpected type (tag = %d)", i)
	}

	if t == nil {
		Fatalf("importer: nil type (type tag = %d)", i)
	}

	return t
}

func (p *importer) qualifiedName() *Sym {
	name := p.string()
	pkg := p.pkg()
	return pkg.Lookup(name)
}

// parser.go:hidden_structdcl_list
func (p *importer) fieldList() []*Node {
	i := p.int()
	if i == 0 {
		return nil
	}
	n := make([]*Node, i)
	for i := range n {
		n[i] = p.field()
	}
	return n
}

// parser.go:hidden_structdcl
func (p *importer) field() *Node {
	sym := p.fieldName()
	typ := p.typ()
	note := p.note()

	var n *Node
	if sym.Name != "" {
		n = Nod(ODCLFIELD, newname(sym), typenod(typ))
	} else {
		// anonymous field - typ must be T or *T and T must be a type name
		s := typ.Sym
		if s == nil && typ.IsPtr() {
			s = typ.Elem().Sym // deref
		}
		pkg := importpkg
		if sym != nil {
			pkg = sym.Pkg
		}
		n = embedded(s, pkg)
		n.Right = typenod(typ)
	}
	n.SetVal(note)

	return n
}

func (p *importer) note() (v Val) {
	if s := p.string(); s != "" {
		v.U = s
	}
	return
}

// parser.go:hidden_interfacedcl_list
func (p *importer) methodList() []*Node {
	i := p.int()
	if i == 0 {
		return nil
	}
	n := make([]*Node, i)
	for i := range n {
		n[i] = p.method()
	}
	return n
}

// parser.go:hidden_interfacedcl
func (p *importer) method() *Node {
	sym := p.fieldName()
	params := p.paramList()
	result := p.paramList()
	return Nod(ODCLFIELD, newname(sym), typenod(functype(fakethis(), params, result)))
}

// parser.go:sym,hidden_importsym
func (p *importer) fieldName() *Sym {
	name := p.string()
	pkg := localpkg
	if name == "_" {
		// During imports, unqualified non-exported identifiers are from builtinpkg
		// (see parser.go:sym). The binary exporter only exports blank as a non-exported
		// identifier without qualification.
		pkg = localpkg
	} else if name == "?" || name != "" && !exportname(name) {
		if name == "?" {
			name = ""
		}
		pkg = p.pkg()
	}
	return pkg.Lookup(name)
}

// parser.go:ohidden_funarg_list
func (p *importer) paramList() []*Node {
	i := p.int()
	if i == 0 {
		return nil
	}
	// negative length indicates unnamed parameters
	named := true
	if i < 0 {
		i = -i
		named = false
	}
	// i > 0
	n := make([]*Node, i)
	for i := range n {
		n[i] = p.param(named)
	}
	return n
}

// parser.go:hidden_funarg
func (p *importer) param(named bool) *Node {
	typ := p.typ()

	isddd := false
	if typ.Etype == TDDDFIELD {
		// TDDDFIELD indicates wrapped ... slice type
		typ = typSlice(typ.DDDField())
		isddd = true
	}

	n := Nod(ODCLFIELD, nil, typenod(typ))
	n.Isddd = isddd

	if named {
		name := p.string()
		if name == "" {
			Fatalf("importer: expected named parameter")
		}
		// TODO(gri) Supply function/method package rather than
		// encoding the package for each parameter repeatedly.
		pkg := p.pkg()
		n.Left = newname(pkg.Lookup(name))
	}

	// TODO(gri) This is compiler-specific (escape info).
	// Move into compiler-specific section eventually?
	n.SetVal(p.note())

	return n
}

func (p *importer) value(typ *Type) (x Val) {
	switch tag := p.tagOrIndex(); tag {
	case falseTag:
		x.U = false

	case trueTag:
		x.U = true

	case int64Tag:
		u := new(Mpint)
		u.SetInt64(p.int64())
		u.Rune = typ == idealrune
		x.U = u

	case floatTag:
		f := newMpflt()
		p.float(f)
		if typ == idealint || typ.IsInteger() {
			// uncommon case: large int encoded as float
			u := new(Mpint)
			u.SetFloat(f)
			x.U = u
			break
		}
		x.U = f

	case complexTag:
		u := new(Mpcplx)
		p.float(&u.Real)
		p.float(&u.Imag)
		x.U = u

	case stringTag:
		x.U = p.string()

	case unknownTag:
		Fatalf("importer: unknown constant (importing package with errors)")

	case nilTag:
		x.U = new(NilVal)

	default:
		Fatalf("importer: unexpected value tag %d", tag)
	}

	// verify ideal type
	if typ.IsUntyped() && untype(x.Ctype()) != typ {
		Fatalf("importer: value %v and type %v don't match", x, typ)
	}

	return
}

func (p *importer) float(x *Mpflt) {
	sign := p.int()
	if sign == 0 {
		x.SetFloat64(0)
		return
	}

	exp := p.int()
	mant := new(big.Int).SetBytes([]byte(p.string()))

	m := x.Val.SetInt(mant)
	m.SetMantExp(m, exp-mant.BitLen())
	if sign < 0 {
		m.Neg(m)
	}
}

// ----------------------------------------------------------------------------
// Inlined function bodies

// Approach: Read nodes and use them to create/declare the same data structures
// as done originally by the (hidden) parser by closely following the parser's
// original code. In other words, "parsing" the import data (which happens to
// be encoded in binary rather textual form) is the best way at the moment to
// re-establish the syntax tree's invariants. At some future point we might be
// able to avoid this round-about way and create the rewritten nodes directly,
// possibly avoiding a lot of duplicate work (name resolution, type checking).

func (p *importer) stmtList() []*Node {
	var list []*Node
	for {
		n := p.node()
		if n == nil {
			break
		}
		// OBLOCK nodes may be created when importing ODCL nodes - unpack them
		if n.Op == OBLOCK {
			list = append(list, n.List.Slice()...)
		} else {
			list = append(list, n)
		}
	}
	return list
}

func (p *importer) exprList() []*Node {
	var list []*Node
	for {
		n := p.expr()
		if n == nil {
			break
		}
		list = append(list, n)
	}
	return list
}

func (p *importer) elemList() []*Node {
	c := p.int()
	list := make([]*Node, c)
	for i := range list {
		list[i] = Nod(OKEY, mkname(p.fieldSym()), p.expr())
	}
	return list
}

func (p *importer) expr() *Node {
	n := p.node()
	if n != nil && n.Op == OBLOCK {
		Fatalf("unexpected block node: %v", n)
	}
	return n
}

// TODO(gri) split into expr and stmt
func (p *importer) node() *Node {
	switch op := p.op(); op {
	// expressions
	// case OPAREN:
	// 	unreachable - unpacked by exporter

	// case ODDDARG:
	//	unimplemented

	// case OREGISTER:
	//	unimplemented

	case OLITERAL:
		typ := p.typ()
		n := nodlit(p.value(typ))
		if !typ.IsUntyped() {
			conv := Nod(OCALL, typenod(typ), nil)
			conv.List.Set1(n)
			n = conv
		}
		return n

	case ONAME:
		if p.bool() {
			// "_"
			// TODO(gri) avoid repeated "_" lookup
			return mkname(Pkglookup("_", localpkg))
		}
		return NodSym(OXDOT, typenod(p.typ()), p.fieldSym())

	case OPACK, ONONAME:
		return mkname(p.sym())

	case OTYPE:
		if p.bool() {
			return mkname(p.sym())
		}
		return typenod(p.typ())

	// case OTARRAY, OTMAP, OTCHAN, OTSTRUCT, OTINTER, OTFUNC:
	//      unreachable - should have been resolved by typechecking

	// case OCLOSURE:
	//	unimplemented

	// case OCOMPLIT:
	//	unimplemented

	case OPTRLIT:
		n := p.expr()
		if !p.bool() /* !implicit, i.e. '&' operator*/ {
			if n.Op == OCOMPLIT {
				// Special case for &T{...}: turn into (*T){...}.
				n.Right = Nod(OIND, n.Right, nil)
				n.Right.Implicit = true
			} else {
				n = Nod(OADDR, n, nil)
			}
		}
		return n

	case OSTRUCTLIT:
		n := Nod(OCOMPLIT, nil, nil)
		if !p.bool() {
			n.Right = typenod(p.typ())
		}
		n.List.Set(p.elemList())
		return n

	case OARRAYLIT, OMAPLIT:
		n := Nod(OCOMPLIT, nil, nil)
		if !p.bool() {
			n.Right = typenod(p.typ())
		}
		n.List.Set(p.exprList())
		return n

	case OKEY:
		left, right := p.exprsOrNil()
		return Nod(OKEY, left, right)

	// case OCALLPART:
	//	unimplemented

	// case OXDOT, ODOT, ODOTPTR, ODOTINTER, ODOTMETH:
	// 	unreachable - mapped to case OXDOT below by exporter

	case OXDOT:
		// see parser.new_dotname
		obj := p.expr()
		sel := p.fieldSym()
		if obj.Op == OPACK {
			s := restrictlookup(sel.Name, obj.Name.Pkg)
			obj.Used = true
			return oldname(s)
		}
		return NodSym(OXDOT, obj, sel)

	// case ODOTTYPE, ODOTTYPE2:
	// 	unreachable - mapped to case ODOTTYPE below by exporter

	case ODOTTYPE:
		n := Nod(ODOTTYPE, p.expr(), nil)
		if p.bool() {
			n.Right = p.expr()
		} else {
			n.Right = typenod(p.typ())
		}
		return n

	// case OINDEX, OINDEXMAP, OSLICE, OSLICESTR, OSLICEARR, OSLICE3, OSLICE3ARR:
	// 	unreachable - mapped to cases below by exporter

	case OINDEX, OSLICE, OSLICE3:
		return Nod(op, p.expr(), p.expr())

	case OCOPY, OCOMPLEX:
		n := builtinCall(op)
		n.List.Set([]*Node{p.expr(), p.expr()})
		return n

	// case OCONV, OCONVIFACE, OCONVNOP, OARRAYBYTESTR, OARRAYRUNESTR, OSTRARRAYBYTE, OSTRARRAYRUNE, ORUNESTR:
	// 	unreachable - mapped to OCONV case below by exporter

	case OCONV:
		n := Nod(OCALL, typenod(p.typ()), nil)
		if p.bool() {
			n.List.Set1(p.expr())
		} else {
			n.List.Set(p.exprList())
		}
		return n

	case OREAL, OIMAG, OAPPEND, OCAP, OCLOSE, ODELETE, OLEN, OMAKE, ONEW, OPANIC, ORECOVER, OPRINT, OPRINTN:
		n := builtinCall(op)
		if p.bool() {
			n.List.Set1(p.expr())
		} else {
			n.List.Set(p.exprList())
			n.Isddd = p.bool()
		}
		return n

	// case OCALL, OCALLFUNC, OCALLMETH, OCALLINTER, OGETG:
	// 	unreachable - mapped to OCALL case below by exporter

	case OCALL:
		n := Nod(OCALL, p.expr(), nil)
		n.List.Set(p.exprList())
		n.Isddd = p.bool()
		return n

	case OMAKEMAP, OMAKECHAN, OMAKESLICE:
		n := builtinCall(OMAKE)
		n.List.Append(typenod(p.typ()))
		n.List.Append(p.exprList()...)
		return n

	// unary expressions
	case OPLUS, OMINUS, OADDR, OCOM, OIND, ONOT, ORECV:
		return Nod(op, p.expr(), nil)

	// binary expressions
	case OADD, OAND, OANDAND, OANDNOT, ODIV, OEQ, OGE, OGT, OLE, OLT,
		OLSH, OMOD, OMUL, ONE, OOR, OOROR, ORSH, OSEND, OSUB, OXOR:
		return Nod(op, p.expr(), p.expr())

	case OADDSTR:
		list := p.exprList()
		x := list[0]
		for _, y := range list[1:] {
			x = Nod(OADD, x, y)
		}
		return x

	// case OCMPSTR, OCMPIFACE:
	// 	unreachable - mapped to std comparison operators by exporter

	case ODCLCONST:
		// TODO(gri) these should not be exported in the first place
		return Nod(OEMPTY, nil, nil)

	// --------------------------------------------------------------------
	// statements
	case ODCL:
		var lhs *Node
		if p.bool() {
			lhs = p.expr()
		} else {
			lhs = dclname(p.sym())
		}
		// TODO(gri) avoid list created here!
		return liststmt(variter([]*Node{lhs}, typenod(p.typ()), nil))

	// case ODCLFIELD:
	//	unimplemented

	case OAS, OASWB:
		if p.bool() {
			lhs := p.expr()
			rhs := p.expr()
			return Nod(OAS, lhs, rhs)
		}
		// TODO(gri) we should not have emitted anything here
		return Nod(OEMPTY, nil, nil)

	case OASOP:
		n := Nod(OASOP, nil, nil)
		n.Etype = EType(p.int())
		n.Left = p.expr()
		if !p.bool() {
			n.Right = Nodintconst(1)
			n.Implicit = true
		} else {
			n.Right = p.expr()
		}
		return n

	case OAS2:
		lhs := p.exprList()
		rhs := p.exprList()
		n := Nod(OAS2, nil, nil)
		n.List.Set(lhs)
		n.Rlist.Set(rhs)
		return n

	case OAS2DOTTYPE, OAS2FUNC, OAS2MAPR, OAS2RECV:
		n := Nod(OAS2, nil, nil)
		n.List.Set(p.exprList())
		n.Rlist.Set(p.exprList())
		return n

	case ORETURN:
		n := Nod(ORETURN, nil, nil)
		n.List.Set(p.exprList())
		return n

	// case ORETJMP:
	// 	unreachable - generated by compiler for trampolin routines (not exported)

	case OPROC, ODEFER:
		return Nod(op, p.expr(), nil)

	case OIF:
		markdcl()
		n := Nod(OIF, nil, nil)
		n.Ninit.Set(p.stmtList())
		n.Left = p.expr()
		n.Nbody.Set(p.stmtList())
		n.Rlist.Set(p.stmtList())
		popdcl()
		return n

	case OFOR:
		markdcl()
		n := Nod(OFOR, nil, nil)
		n.Ninit.Set(p.stmtList())
		n.Left, n.Right = p.exprsOrNil()
		n.Nbody.Set(p.stmtList())
		popdcl()
		return n

	case ORANGE:
		markdcl()
		n := Nod(ORANGE, nil, nil)
		n.List.Set(p.stmtList())
		n.Right = p.expr()
		n.Nbody.Set(p.stmtList())
		popdcl()
		return n

	case OSELECT, OSWITCH:
		markdcl()
		n := Nod(op, nil, nil)
		n.Ninit.Set(p.stmtList())
		n.Left, _ = p.exprsOrNil()
		n.List.Set(p.stmtList())
		popdcl()
		return n

	case OCASE, OXCASE:
		markdcl()
		n := Nod(OXCASE, nil, nil)
		n.List.Set(p.exprList())
		// TODO(gri) eventually we must declare variables for type switch
		// statements (type switch statements are not yet exported)
		n.Nbody.Set(p.stmtList())
		popdcl()
		return n

	case OBREAK, OCONTINUE, OGOTO, OFALL, OXFALL:
		if op == OFALL {
			op = OXFALL
		}
		left, _ := p.exprsOrNil()
		return Nod(op, left, nil)

	// case OEMPTY:
	// 	unreachable - not emitted by exporter

	case OLABEL:
		n := Nod(OLABEL, p.expr(), nil)
		n.Left.Sym = dclstack // context, for goto restrictions
		return n

	case OEND:
		return nil

	default:
		Fatalf("importer: %s (%d) node not yet supported", opnames[op], op)
		panic("unreachable") // satisfy compiler
	}
}

func builtinCall(op Op) *Node {
	return Nod(OCALL, mkname(builtinpkg.Lookup(goopnames[op])), nil)
}

func (p *importer) exprsOrNil() (a, b *Node) {
	ab := p.int()
	if ab&1 != 0 {
		a = p.expr()
	}
	if ab&2 != 0 {
		b = p.expr()
	}
	return
}

func (p *importer) fieldSym() *Sym {
	name := p.string()
	pkg := localpkg
	if !exportname(name) {
		pkg = p.pkg()
	}
	return pkg.Lookup(name)
}

func (p *importer) sym() *Sym {
	name := p.string()
	pkg := localpkg
	if name != "_" {
		pkg = p.pkg()
	}
	return pkg.Lookup(name)
}

func (p *importer) bool() bool {
	return p.int() != 0
}

func (p *importer) op() Op {
	return Op(p.int())
}

// ----------------------------------------------------------------------------
// Low-level decoders

func (p *importer) tagOrIndex() int {
	if p.debugFormat {
		p.marker('t')
	}

	return int(p.rawInt64())
}

func (p *importer) int() int {
	x := p.int64()
	if int64(int(x)) != x {
		Fatalf("importer: exported integer too large")
	}
	return int(x)
}

func (p *importer) int64() int64 {
	if p.debugFormat {
		p.marker('i')
	}

	return p.rawInt64()
}

func (p *importer) string() string {
	if p.debugFormat {
		p.marker('s')
	}

	// TODO(gri) should we intern strings here?

	if n := int(p.rawInt64()); n > 0 {
		if cap(p.buf) < n {
			p.buf = make([]byte, n)
		} else {
			p.buf = p.buf[:n]
		}
		for i := range p.buf {
			p.buf[i] = p.byte()
		}
		return string(p.buf)
	}

	return ""
}

func (p *importer) marker(want byte) {
	if got := p.byte(); got != want {
		Fatalf("importer: incorrect marker: got %c; want %c (pos = %d)", got, want, p.read)
	}

	pos := p.read
	if n := int(p.rawInt64()); n != pos {
		Fatalf("importer: incorrect position: got %d; want %d", n, pos)
	}
}

// rawInt64 should only be used by low-level decoders
func (p *importer) rawInt64() int64 {
	i, err := binary.ReadVarint(p)
	if err != nil {
		Fatalf("importer: read error: %v", err)
	}
	return i
}

// needed for binary.ReadVarint in rawInt64
func (p *importer) ReadByte() (byte, error) {
	return p.byte(), nil
}

// byte is the bottleneck interface for reading from p.in.
// It unescapes '|' 'S' to '$' and '|' '|' to '|'.
func (p *importer) byte() byte {
	c, err := p.in.ReadByte()
	p.read++
	if err != nil {
		Fatalf("importer: read error: %v", err)
	}
	if c == '|' {
		c, err = p.in.ReadByte()
		p.read++
		if err != nil {
			Fatalf("importer: read error: %v", err)
		}
		switch c {
		case 'S':
			c = '$'
		case '|':
			// nothing to do
		default:
			Fatalf("importer: unexpected escape sequence in export data")
		}
	}
	return c
}
