package main

import (
	"fmt"
	"strings"

	"github.com/xlab/treeprint"
	_ "github.com/xlab/treeprint"
)

func swap[T any](a []T, i, j int) {
	if i >= 0 && i < len(a) && j >= 0 && j < len(a) {
		t := a[i]
		a[i] = a[j]
		a[j] = t
	}
}

func pop[T any](a []T) []T {
	if len(a) > 0 {
		return a[:len(a)-1]
	}
	return a
}

func copy[T any](src []T) []T {
	dst := make([]T, len(src))
	for i := range src {
		dst[i] = src[i]
	}
	return dst
}

const (
	defaultIndent = 4
)

type FormatCtx struct {
	buf    strings.Builder
	line   strings.Builder
	offset Padding
	indent Padding
}

func (fc *FormatCtx) AddOffset() {
	fc.offset.AddPad()
}

func (fc *FormatCtx) RestoreOffset() {
	fc.offset.RestorePad()
}

func (fc *FormatCtx) fillOffset() {
	for i := 0; i < fc.offset.pad; i++ {
		fc.buf.WriteByte(' ')
	}
}

func (fc *FormatCtx) fillIndent() {
	for i := 0; i < fc.indent.pad; i++ {
		fc.buf.WriteByte(' ')
	}
}

func (fc *FormatCtx) writeString(s string) {
	for _, c := range s {
		fc.line.WriteRune(c)
		if c == '\n' {
			fc.buf.WriteString(fc.line.String())
			fc.fillOffset()
			fc.line.Reset()
		}
	}
	fc.buf.WriteString(fc.line.String())
	fc.line.Reset()
}

func (fc *FormatCtx) String() string {
	return fc.buf.String()
}

func (fc *FormatCtx) Write(s string) {
	fc.writeString(s)
}

func (fc *FormatCtx) Writef(f string, args ...any) {
	fc.writeString(fmt.Sprintf(f, args...))
}

func (fc *FormatCtx) Writefln(f string, args ...any) {
	fc.writeString(fmt.Sprintf(f+"\n", args...))
}

func (fc *FormatCtx) Writeln(args ...any) {
	fc.writeString(fmt.Sprintln(args...))
}

func (fc *FormatCtx) WriteStrings(ss []string) {
	for _, s := range ss {
		fc.Writeln(s)
	}
}

func WriteMap[K comparable, V any](ctx *FormatCtx, m map[K]V) {
	for k, v := range m {
		ctx.Writeln(k, ":", v)
	}
}

func WriteExprs(ctx *FormatCtx, exprs []*Expr) {
	for _, e := range exprs {
		e.Format(ctx)
		ctx.Writeln()
	}
}

func WriteExpr(ctx *FormatCtx, expr *Expr) {
	expr.Format(ctx)
	ctx.Writeln()
}

func WriteMapTree[K comparable, V any](tree treeprint.Tree, m map[K]V) {
	for k, v := range m {
		tree.AddNode(fmt.Sprintf("%v : %v", k, v))
	}
}

func WriteExprsTree(tree treeprint.Tree, exprs []*Expr) {
	for i, e := range exprs {
		p := tree.AddBranch(fmt.Sprintf("%d", i))
		e.Print(p)
	}
}

func WriteExprTree(tree treeprint.Tree, expr *Expr) {
	expr.Print(tree)
}

type Format interface {
	Format(*FormatCtx)
}

type Padding struct {
	pad      int
	lastPads []int
}

func (fc *Padding) addPad(d int) {
	if fc.pad+d < 0 {
		fc.pad = 0
	} else {
		fc.pad += d
	}
}

func (fc *Padding) AddPad() {
	fc.lastPads = append(fc.lastPads, fc.pad)
	fc.addPad(defaultIndent)
}

func (fc *Padding) RestorePad() {
	if len(fc.lastPads) <= 0 {
		fc.addPad(-fc.pad)
		return
	}
	fc.pad = fc.lastPads[len(fc.lastPads)-1]
	fc.lastPads = pop(fc.lastPads)
}

func listExprs(bb *strings.Builder, exprs []*Expr) *strings.Builder {
	for i, expr := range exprs {
		bb.WriteString(fmt.Sprintf("\n  %d: ", i))
		bb.WriteString(expr.String())
	}
	return bb
}

func Min[T int](a, b T) T {
	if a > b {
		return b
	}
	return a
}

func isDisjoint(a, b map[uint64]bool) bool {
	for k, _ := range a {
		if _, has := b[k]; has {
			return false
		}
	}
	return true
}
