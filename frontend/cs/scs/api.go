// Copyright 2020-2025 Consensys Software Inc.
// Licensed under the Apache License, Version 2.0. See the LICENSE file for details.

package scs

import (
	"fmt"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"

	"github.com/consensys/gnark/debug"
	"github.com/consensys/gnark/frontend/cs"

	"github.com/consensys/gnark/constraint"
	"github.com/consensys/gnark/constraint/solver"
	"github.com/consensys/gnark/frontend"
	"github.com/consensys/gnark/frontend/internal/expr"
	"github.com/consensys/gnark/frontend/schema"
	"github.com/consensys/gnark/internal/frontendtype"
	"github.com/consensys/gnark/internal/gkr/gkrinfo"
	"github.com/consensys/gnark/internal/smallfields"
	"github.com/consensys/gnark/std/math/bits"
)

func (builder *builder[E]) Add(i1, i2 frontend.Variable, in ...frontend.Variable) frontend.Variable {
	// separate the constant part from the variables
	vars, k := builder.filterConstantSum(append([]frontend.Variable{i1, i2}, in...))

	if len(vars) == 0 {
		// no variables, we return the constant.
		return builder.cs.ToBigInt(k)
	}

	vars = builder.reduce(vars)
	if k.IsZero() {
		return builder.splitSum(vars[0], vars[1:], nil)
	}
	// no constant we decompose the linear expressions in additions of 2 terms
	return builder.splitSum(vars[0], vars[1:], &k)
}

func (builder *builder[E]) MulAcc(a, b, c frontend.Variable) frontend.Variable {

	if fastTrack := builder.mulAccFastTrack(a, b, c); fastTrack != nil {
		return fastTrack
	}

	// TODO can we do better here to limit allocations?
	return builder.Add(a, builder.Mul(b, c))
}

// special case for when a/c is constant
// let a = a' * α, b = b' * β, c = c' * α
// then a + b * c = a' * α + (b' * c') (β * α)
// thus qL = a', qR = 0, qM = b'c'
func (builder *builder[E]) mulAccFastTrack(a, b, c frontend.Variable) frontend.Variable {
	var (
		aVar, bVar, cVar expr.Term[E]
		ok               bool
	)
	if aVar, ok = a.(expr.Term[E]); !ok {
		return nil
	}
	if bVar, ok = b.(expr.Term[E]); !ok {
		return nil
	}
	if cVar, ok = c.(expr.Term[E]); !ok {
		return nil
	}

	if aVar.VID == bVar.VID {
		bVar, cVar = cVar, bVar
	}

	if aVar.VID != cVar.VID {
		return nil
	}

	res := builder.newInternalVariable()
	var zero E
	builder.addPlonkConstraint(sparseR1C[E]{
		xa:         aVar.VID,
		xb:         bVar.VID,
		xc:         res.VID,
		qL:         aVar.Coeff,
		qR:         zero,
		qO:         builder.tMinusOne,
		qM:         builder.cs.Mul(bVar.Coeff, cVar.Coeff),
		qC:         zero,
		commitment: 0,
	})
	return res
}

func (builder *builder[E]) neg(in []frontend.Variable) []frontend.Variable {
	res := make([]frontend.Variable, len(in))

	for i := 0; i < len(in); i++ {
		res[i] = builder.Neg(in[i])
	}
	return res
}

func (builder *builder[E]) Sub(i1, i2 frontend.Variable, in ...frontend.Variable) frontend.Variable {
	r := builder.neg(append([]frontend.Variable{i2}, in...))
	return builder.Add(i1, r[0], r[1:]...)
}

func (builder *builder[E]) Neg(i1 frontend.Variable) frontend.Variable {
	if n, ok := builder.constantValue(i1); ok {
		n = builder.cs.Neg(n)
		return builder.cs.ToBigInt(n)
	}
	v := i1.(expr.Term[E])
	v.Coeff = builder.cs.Neg(v.Coeff)
	return v
}

func (builder *builder[E]) Mul(i1, i2 frontend.Variable, in ...frontend.Variable) frontend.Variable {
	vars, k := builder.filterConstantProd(append([]frontend.Variable{i1, i2}, in...))
	if len(vars) == 0 {
		return builder.cs.ToBigInt(k)
	}
	if k.IsZero() {
		return 0
	}
	for i := range vars {
		if vars[i].Coeff.IsZero() {
			return 0
		}
	}
	l := builder.mulConstant(vars[0], k)

	return builder.splitProd(l, vars[1:])
}

// returns t*m
func (builder *builder[E]) mulConstant(t expr.Term[E], m E) expr.Term[E] {
	t.Coeff = builder.cs.Mul(t.Coeff, m)
	return t
}

func (builder *builder[E]) DivUnchecked(i1, i2 frontend.Variable) frontend.Variable {
	c1, i1Constant := builder.constantValue(i1)
	c2, i2Constant := builder.constantValue(i2)

	if i1Constant && i2Constant {
		if c2.IsZero() {
			panic("inverse by constant(0)")
		}
		c2, _ = builder.cs.Inverse(c2)
		c2 = builder.cs.Mul(c2, c1)
		return builder.cs.ToBigInt(c2)
	}
	if i2Constant {
		if c2.IsZero() {
			panic("inverse by constant(0)")
		}
		c2, _ = builder.cs.Inverse(c2)
		return builder.mulConstant(i1.(expr.Term[E]), c2)
	}
	if i1Constant {
		res := builder.Inverse(i2)
		return builder.mulConstant(res.(expr.Term[E]), c1)
	}

	// res * i2 == i1
	res := builder.newInternalVariable()
	builder.addPlonkConstraint(sparseR1C[E]{
		xa: res.VID,
		xb: i2.(expr.Term[E]).VID,
		xc: i1.(expr.Term[E]).VID,
		qM: i2.(expr.Term[E]).Coeff,
		qO: builder.cs.Neg(i1.(expr.Term[E]).Coeff),
	})

	return res
}

func (builder *builder[E]) Div(i1, i2 frontend.Variable) frontend.Variable {
	// note that here we ensure that v2 can't be 0, but it costs us one extra constraint
	builder.Inverse(i2)

	return builder.DivUnchecked(i1, i2)
}

func (builder *builder[E]) Inverse(i1 frontend.Variable) frontend.Variable {
	if c, ok := builder.constantValue(i1); ok {
		if c.IsZero() {
			panic("inverse by constant(0)")
		}
		c, _ = builder.cs.Inverse(c)
		return builder.cs.ToBigInt(c)
	}
	t := i1.(expr.Term[E])
	res := builder.newInternalVariable()

	// res * i1 - 1 == 0
	constraint := sparseR1C[E]{
		xa: res.VID,
		xb: t.VID,
		qM: t.Coeff,
		qC: builder.tMinusOne,
	}

	if debug.Debug {
		debug := builder.newDebugInfo("inverse", "1/", i1, " < ∞")
		builder.addPlonkConstraint(constraint, debug)
	} else {
		builder.addPlonkConstraint(constraint)
	}

	return res
}

// ---------------------------------------------------------------------------------------------
// Bit operations

func (builder *builder[E]) ToBinary(i1 frontend.Variable, n ...int) []frontend.Variable {
	// nbBits
	nbBits := builder.cs.FieldBitLen()
	if len(n) == 1 {
		nbBits = n[0]
		if nbBits < 0 {
			panic("invalid n")
		}
	}

	return bits.ToBinary(builder, i1, bits.WithNbDigits(nbBits))
}

func (builder *builder[E]) FromBinary(b ...frontend.Variable) frontend.Variable {
	return bits.FromBinary(builder, b)
}

func (builder *builder[E]) Xor(a, b frontend.Variable) frontend.Variable {
	// pre condition: a, b must be booleans
	builder.AssertIsBoolean(a)
	builder.AssertIsBoolean(b)

	_a, aConstant := builder.constantValue(a)
	_b, bConstant := builder.constantValue(b)

	// if both inputs are constants
	if aConstant && bConstant {
		b0 := 0
		b1 := 0
		if builder.cs.IsOne(_a) {
			b0 = 1
		}
		if builder.cs.IsOne(_b) {
			b1 = 1
		}
		return b0 ^ b1
	}

	res := builder.newInternalVariable()
	builder.MarkBoolean(res)

	// if one input is constant, ensure we put it in b.
	if aConstant {
		a, b = b, a
		bConstant = aConstant
		_b = _a
	}
	if bConstant {
		xa := a.(expr.Term[E])
		// 1 - 2b
		qL := builder.tOne
		qL = builder.cs.Sub(qL, _b)
		qL = builder.cs.Sub(qL, _b)
		qL = builder.cs.Mul(qL, xa.Coeff)

		// (1-2b)a + b == res
		builder.addPlonkConstraint(sparseR1C[E]{
			xa: xa.VID,
			xc: res.VID,
			qL: qL,
			qO: builder.tMinusOne,
			qC: _b,
		})
		// builder.addPlonkConstraint(xa, xb, res, builder.st.CoeffID(oneMinusTwoB), constraint.CoeffIdZero, constraint.CoeffIdZero, constraint.CoeffIdZero, constraint.CoeffIdMinusOne, builder.st.CoeffID(_b))
		return res
	}
	xa := a.(expr.Term[E])
	xb := b.(expr.Term[E])

	// -a - b + 2ab + res == 0
	qM := builder.tOne
	qM = builder.cs.Add(qM, qM)
	qM = builder.cs.Mul(qM, xa.Coeff)
	qM = builder.cs.Mul(qM, xb.Coeff)

	qL := builder.cs.Neg(xa.Coeff)
	qR := builder.cs.Neg(xb.Coeff)

	builder.addPlonkConstraint(sparseR1C[E]{
		xa: xa.VID,
		xb: xb.VID,
		xc: res.VID,
		qL: qL,
		qR: qR,
		qO: builder.tOne,
		qM: qM,
	})
	// builder.addPlonkConstraint(xa, xb, res, constraint.CoeffIdMinusOne, constraint.CoeffIdMinusOne, constraint.CoeffIdTwo, constraint.CoeffIdOne, constraint.CoeffIdOne, constraint.CoeffIdZero)
	return res
}

func (builder *builder[E]) Or(a, b frontend.Variable) frontend.Variable {
	builder.AssertIsBoolean(a)
	builder.AssertIsBoolean(b)

	_a, aConstant := builder.constantValue(a)
	_b, bConstant := builder.constantValue(b)

	if aConstant && bConstant {
		if builder.cs.IsOne(_a) || builder.cs.IsOne(_b) {
			return 1
		}
		return 0
	}

	// if one input is constant, ensure we put it in b
	if aConstant {
		a, b = b, a
		_b = _a
		bConstant = aConstant
	}

	if bConstant {
		if builder.cs.IsOne(_b) {
			return 1
		} else {
			return a
		}
	}
	res := builder.newInternalVariable()
	builder.MarkBoolean(res)
	xa := a.(expr.Term[E])
	xb := b.(expr.Term[E])
	// -a - b + ab + res == 0

	qM := builder.cs.Mul(xa.Coeff, xb.Coeff)

	qL := builder.cs.Neg(xa.Coeff)
	qR := builder.cs.Neg(xb.Coeff)

	builder.addPlonkConstraint(sparseR1C[E]{
		xa: xa.VID,
		xb: xb.VID,
		xc: res.VID,
		qL: qL,
		qR: qR,
		qM: qM,
		qO: builder.tOne,
	})
	return res
}

func (builder *builder[E]) And(a, b frontend.Variable) frontend.Variable {
	builder.AssertIsBoolean(a)
	builder.AssertIsBoolean(b)
	res := builder.Mul(a, b)
	builder.MarkBoolean(res)
	return res
}

// ---------------------------------------------------------------------------------------------
// Conditionals

func (builder *builder[E]) Select(b frontend.Variable, i1, i2 frontend.Variable) frontend.Variable {
	_b, bConstant := builder.constantValue(b)

	if bConstant {
		if !builder.IsBoolean(b) {
			panic(fmt.Sprintf("%s should be 0 or 1", builder.cs.String(_b)))
		}
		if _b.IsZero() {
			return i2
		}
		return i1
	}

	// ensure the condition is a boolean
	builder.AssertIsBoolean(b)

	u := builder.Sub(i1, i2)
	l := builder.Mul(u, b)

	return builder.Add(l, i2)
}

func (builder *builder[E]) Lookup2(b0, b1 frontend.Variable, i0, i1, i2, i3 frontend.Variable) frontend.Variable {
	// ensure that bits are actually bits. Adds no constraints if the variables
	// are already constrained.
	builder.AssertIsBoolean(b0)
	builder.AssertIsBoolean(b1)

	c0, b0IsConstant := builder.constantValue(b0)
	c1, b1IsConstant := builder.constantValue(b1)

	if b0IsConstant && b1IsConstant {
		b0 := builder.cs.IsOne(c0)
		b1 := builder.cs.IsOne(c1)

		if !b0 && !b1 {
			return i0
		}
		if b0 && !b1 {
			return i1
		}
		if b0 && b1 {
			return i3
		}
		return i2
	}

	// two-bit lookup for the general case can be done with three constraints as
	// following:
	//    (1) (in3 - in2 - in1 + in0) * s1 = tmp1 - in1 + in0
	//    (2) tmp1 * s0 = tmp2
	//    (3) (in2 - in0) * s1 = RES - tmp2 - in0
	// the variables tmp1 and tmp2 are new internal variables and the variables
	// RES will be the returned result
	tmp1 := builder.Sub(i3, i2)
	tmp := builder.Sub(i0, i1)
	tmp1 = builder.Add(tmp1, tmp)
	tmp1 = builder.Mul(tmp1, b1)
	tmp1 = builder.Sub(tmp1, tmp) // (1) tmp1 = s1 * (in3 - in2 - in1 + in0) + in1 - in0
	tmp2 := builder.Mul(tmp1, b0) // (2) tmp2 = tmp1 * s0
	res := builder.Sub(i2, i0)
	res = builder.Mul(res, b1)
	res = builder.Add(res, tmp2, i0) // (3) res = (v2 - v0) * s1 + tmp2 + in0

	return res

}

func (builder *builder[E]) IsZero(i1 frontend.Variable) frontend.Variable {
	if a, ok := builder.constantValue(i1); ok {
		if a.IsZero() {
			return 1
		}
		return 0
	}

	// x = 1/a 				// in a hint (x == 0 if a == 0)
	// m = -a*x + 1         // constrain m to be 1 if a == 0
	// a * m = 0            // constrain m to be 0 if a != 0
	a := i1.(expr.Term[E])
	m := builder.newInternalVariable()

	// x = 1/a 				// in a hint (x == 0 if a == 0)
	x, err := builder.NewHint(solver.InvZeroHint, 1, a)
	if err != nil {
		// the function errs only if the number of inputs is invalid.
		panic(err)
	}

	// m = -a*x + 1         // constrain m to be 1 if a == 0
	// a*x + m - 1 == 0
	X := x[0].(expr.Term[E])
	builder.addPlonkConstraint(sparseR1C[E]{
		xa: a.VID,
		xb: X.VID,
		xc: m.VID,
		qM: a.Coeff,
		qO: builder.tOne,
		qC: builder.tMinusOne,
	})

	// a * m = 0            // constrain m to be 0 if a != 0
	builder.addPlonkConstraint(sparseR1C[E]{
		xa: a.VID,
		xb: m.VID,
		qM: a.Coeff,
	})

	builder.MarkBoolean(m)

	return m
}

func (builder *builder[E]) Cmp(i1, i2 frontend.Variable) frontend.Variable {

	nbBits := builder.cs.FieldBitLen()
	// in AssertIsLessOrEq we omitted comparison against modulus for the left
	// side as if `a+r<b` implies `a<b`, then here we compute the inequality
	// directly.
	bi1 := bits.ToBinary(builder, i1, bits.WithNbDigits(nbBits))
	bi2 := bits.ToBinary(builder, i2, bits.WithNbDigits(nbBits))

	var res frontend.Variable
	res = 0

	for i := builder.cs.FieldBitLen() - 1; i >= 0; i-- {
		iszeroi1 := builder.IsZero(bi1[i])
		iszeroi2 := builder.IsZero(bi2[i])

		i1i2 := builder.And(bi1[i], iszeroi2)
		i2i1 := builder.And(bi2[i], iszeroi1)

		n := builder.Select(i2i1, -1, 0)
		m := builder.Select(i1i2, 1, n)

		res = builder.Select(builder.IsZero(res), m, res)
	}
	return res
}

func (builder *builder[E]) Println(a ...frontend.Variable) {
	var log constraint.LogEntry

	// prefix log line with file.go:line
	if _, file, line, ok := runtime.Caller(1); ok {
		log.Caller = fmt.Sprintf("%s:%d", filepath.Base(file), line)
	}

	var sbb strings.Builder

	for i, arg := range a {
		if i > 0 {
			sbb.WriteByte(' ')
		}
		if v, ok := arg.(expr.Term[E]); ok {

			sbb.WriteString("%s")
			// we set limits to the linear expression, so that the log printer
			// can evaluate it before printing it
			log.ToResolve = append(log.ToResolve, constraint.LinearExpression{builder.cs.MakeTerm(v.Coeff, v.VID)})
		} else {
			builder.printArg(&log, &sbb, arg)
		}
	}

	// set format string to be used with fmt.Sprintf, once the variables are solved in the R1CS.Solve() method
	log.Format = sbb.String()

	builder.cs.AddLog(log)
}

func (builder *builder[E]) printArg(log *constraint.LogEntry, sbb *strings.Builder, a frontend.Variable) {

	leafCount, err := schema.Walk(builder.Field(), a, tVariable, nil)
	count := leafCount.Public + leafCount.Secret

	// no variables in nested struct, we use fmt std print function
	if count == 0 || err != nil {
		sbb.WriteString(fmt.Sprint(a))
		return
	}

	sbb.WriteByte('{')
	printer := func(f schema.LeafInfo, tValue reflect.Value) error {
		count--
		sbb.WriteString(f.FullName())
		sbb.WriteString(": ")
		sbb.WriteString("%s")
		if count != 0 {
			sbb.WriteString(", ")
		}

		v := tValue.Interface().(expr.Term[E])
		// we set limits to the linear expression, so that the log printer
		// can evaluate it before printing it
		log.ToResolve = append(log.ToResolve, constraint.LinearExpression{builder.cs.MakeTerm(v.Coeff, v.VID)})
		return nil
	}
	// ignoring error, printer() doesn't return errors
	_, _ = schema.Walk(builder.Field(), a, tVariable, printer)
	sbb.WriteByte('}')
}

func (builder *builder[E]) Compiler() frontend.Compiler {
	return builder
}

func (builder *builder[E]) Commit(v ...frontend.Variable) (frontend.Variable, error) {
	if smallfields.IsSmallField(builder.Field()) {
		return nil, fmt.Errorf("commitment not supported for small field %s", builder.Field())
	}

	commitments := builder.cs.GetCommitments().(constraint.PlonkCommitments)
	v = filterConstants[E](v) // TODO: @Tabaie Settle on a way to represent even constants; conventional hash?

	committed := make([]int, len(v))

	for i, vI := range v { // TODO @Tabaie Perf; If public, just hash it
		vINeg := builder.Neg(vI).(expr.Term[E])
		committed[i] = builder.cs.GetNbConstraints()
		// a constraint to enforce consistency between the commitment and committed value
		// - v + comm(n) = 0
		builder.addPlonkConstraint(sparseR1C[E]{xa: vINeg.VID, qL: vINeg.Coeff, commitment: constraint.COMMITTED})
	}

	inputs := make([]frontend.Variable, len(v)+1)
	inputs[0] = len(commitments) // commitment depth
	copy(inputs[1:], v)
	outs, err := builder.NewHint(cs.Bsb22CommitmentComputePlaceholder, 1, inputs...)
	if err != nil {
		return nil, err
	}

	commitmentVar := builder.Neg(outs[0]).(expr.Term[E])
	commitmentConstraintIndex := builder.cs.GetNbConstraints()
	// RHS will be provided by both prover and verifier independently, as for a public wire
	builder.addPlonkConstraint(sparseR1C[E]{xa: commitmentVar.VID, qL: commitmentVar.Coeff, commitment: constraint.COMMITMENT}) // value will be injected later

	return outs[0], builder.cs.AddCommitment(constraint.PlonkCommitment{
		CommitmentIndex: commitmentConstraintIndex,
		Committed:       committed,
	})
}

// EvaluatePlonkExpression in the form of res = qL.a + qR.b + qM.ab + qC
func (builder *builder[E]) EvaluatePlonkExpression(a, b frontend.Variable, qL, qR, qM, qC int) frontend.Variable {
	_, aConstant := builder.constantValue(a)
	_, bConstant := builder.constantValue(b)
	if aConstant || bConstant {
		return builder.Add(
			builder.Mul(a, qL),
			builder.Mul(b, qR),
			builder.Mul(a, b, qM),
			qC,
		)
	}

	res := builder.newInternalVariable()
	builder.addPlonkConstraint(sparseR1C[E]{
		xa: a.(expr.Term[E]).VID,
		xb: b.(expr.Term[E]).VID,
		xc: res.VID,
		qL: builder.cs.Mul(builder.cs.FromInterface(qL), a.(expr.Term[E]).Coeff),
		qR: builder.cs.Mul(builder.cs.FromInterface(qR), b.(expr.Term[E]).Coeff),
		qO: builder.tMinusOne,
		qM: builder.cs.Mul(builder.cs.FromInterface(qM), builder.cs.Mul(a.(expr.Term[E]).Coeff, b.(expr.Term[E]).Coeff)),
		qC: builder.cs.FromInterface(qC),
	})
	return res
}

// AddPlonkConstraint asserts qL.a + qR.b + qO.o + qM.ab + qC = 0
func (builder *builder[E]) AddPlonkConstraint(a, b, o frontend.Variable, qL, qR, qO, qM, qC int) {
	_, aConstant := builder.constantValue(a)
	_, bConstant := builder.constantValue(b)
	_, oConstant := builder.constantValue(o)
	if aConstant || bConstant || oConstant {
		builder.AssertIsEqual(
			builder.Add(
				builder.Mul(a, qL),
				builder.Mul(b, qR),
				builder.Mul(a, b, qM),
				builder.Mul(o, qO),
				qC,
			),
			0,
		)
		return
	}

	builder.addPlonkConstraint(sparseR1C[E]{
		xa: a.(expr.Term[E]).VID,
		xb: b.(expr.Term[E]).VID,
		xc: o.(expr.Term[E]).VID,
		qL: builder.cs.Mul(builder.cs.FromInterface(qL), a.(expr.Term[E]).Coeff),
		qR: builder.cs.Mul(builder.cs.FromInterface(qR), b.(expr.Term[E]).Coeff),
		qO: builder.cs.Mul(builder.cs.FromInterface(qO), o.(expr.Term[E]).Coeff),
		qM: builder.cs.Mul(builder.cs.FromInterface(qM), builder.cs.Mul(a.(expr.Term[E]).Coeff, b.(expr.Term[E]).Coeff)),
		qC: builder.cs.FromInterface(qC),
	})
}

func filterConstants[E constraint.Element](v []frontend.Variable) []frontend.Variable {
	res := make([]frontend.Variable, 0, len(v))
	for _, vI := range v {
		if _, ok := vI.(expr.Term[E]); ok {
			res = append(res, vI)
		}
	}
	return res
}

func (*builder[E]) FrontendType() frontendtype.Type {
	return frontendtype.SCS
}

func (builder *builder[E]) SetGkrInfo(info gkrinfo.StoringInfo) error {
	return builder.cs.AddGkr(info)
}
