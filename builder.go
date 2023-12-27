package main

import (
	"errors"
	"fmt"

	"github.com/xlab/treeprint"
)

type BindingType int

const (
	BT_BASE BindingType = iota
	BT_TABLE
	BT_DUMMY
	BT_CATALOG_ENTRY
)

func (bt BindingType) String() string {
	switch bt {
	case BT_BASE:
		return "base"
	case BT_TABLE:
		return "table"
	case BT_DUMMY:
		return "dummy"
	case BT_CATALOG_ENTRY:
		return "catalog_entry"
	default:
		panic(fmt.Sprintf("usp binding type %d", bt))
	}
}

type Binding struct {
	typ      BindingType
	database string
	alias    string
	index    uint64
	typs     []ExprDataType
	names    []string
	nameMap  map[string]int
}

func (b *Binding) Format(ctx *FormatCtx) {
	ctx.Writefln("%s.%s,%s,%d", b.database, b.alias, b.typ, b.index)
	for i, n := range b.names {
		ctx.Writeln(i, n, b.typs[i])
	}
}

func (b *Binding) Print(tree treeprint.Tree) {
	tree = tree.AddMetaBranch(fmt.Sprintf("%s, %d", b.typ, b.index), fmt.Sprintf("%s.%s", b.database, b.alias))
	sub := tree.AddBranch("columns")
	for i, n := range b.names {
		sub.AddNode(fmt.Sprintf("%d %s %s", i, n, b.typs[i]))
	}
}

func (b *Binding) Bind(table, column string, depth int) (*Expr, error) {
	if idx, ok := b.nameMap[column]; ok {
		exp := &Expr{
			Typ:     ET_Column,
			DataTyp: b.typs[idx],
			Table:   table,
			Name:    column,
			ColRef:  [2]uint64{b.index, uint64(idx)},
			Depth:   depth,
		}
		return exp, nil
	}
	return nil, fmt.Errorf("table %s does not have column %s", table, column)
}

func (b *Binding) HasColumn(column string) int {
	if idx, ok := b.nameMap[column]; ok {
		return idx
	}
	return -1
}

type BindContext struct {
	parent       *BindContext
	bindings     map[string]*Binding
	bindingsList []*Binding
}

func NewBindContext(parent *BindContext) *BindContext {
	return &BindContext{
		parent:   parent,
		bindings: make(map[string]*Binding, 0),
	}
}

func (bc *BindContext) Format(ctx *FormatCtx) {
	for _, b := range bc.bindings {
		b.Format(ctx)
	}
}

func (bc *BindContext) Print(tree treeprint.Tree) {
	for _, b := range bc.bindings {
		b.Print(tree)
	}
}

func (bc *BindContext) AddBinding(alias string, b *Binding) error {
	if _, ok := bc.bindings[alias]; ok {
		return errors.New("duplicate alias " + alias)
	}
	bc.bindingsList = append(bc.bindingsList, b)
	bc.bindings[alias] = b
	return nil
}

func (bc *BindContext) AddContext(obc *BindContext) error {
	for alias, ob := range obc.bindings {
		if _, ok := bc.bindings[alias]; ok {
			return errors.New("duplicate alias " + alias)
		}
		bc.bindings[alias] = ob
	}
	bc.bindingsList = append(bc.bindingsList, obc.bindingsList...)
	return nil
}

func (bc *BindContext) RemoveContext(obList []*Binding) {
	for _, ob := range obList {
		found := -1
		for i, b := range bc.bindingsList {
			if ob.alias == b.alias {
				found = i
				break
			}
		}
		if found > -1 {
			swap(bc.bindingsList, found, len(bc.bindingsList)-1)
			bc.bindingsList = pop(bc.bindingsList)
		}

		delete(bc.bindings, ob.alias)
	}
}

func (bc *BindContext) GetBinding(name string) (*Binding, error) {
	if b, ok := bc.bindings[name]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("table %s does not exists", name)
}

func (bc *BindContext) GetMatchingBinding(column string) (*Binding, int, error) {
	var ret *Binding
	var err error
	var depth int
	for _, b := range bc.bindings {
		if b.HasColumn(column) >= 0 {
			if ret != nil {
				return nil, 0, fmt.Errorf("Ambiguous column %s in %s or %s", column, ret.alias, b.alias)
			}
			ret = b
		}
	}

	//find it in parent context
	parDepth := -1
	for p := bc.parent; p != nil && ret == nil; p = p.parent {
		ret, parDepth, err = p.GetMatchingBinding(column)
		if err != nil {
			return nil, 0, err
		}
	}

	if ret == nil {
		return nil, 0, fmt.Errorf("no table has column %s", column)
	}
	if parDepth != -1 {
		depth = parDepth + 1
	}
	return ret, depth, nil
}

func (bc *BindContext) BindColumn(table, column string, depth int) (*Expr, error) {
	b, err := bc.GetBinding(table)
	if err != nil {
		return nil, err
	}

	return b.Bind(table, column, depth)
}

var _ Format = &Builder{}

type Builder struct {
	tag        int // relation tag
	projectTag int
	groupTag   int
	aggTag     int
	rootCtx    *BindContext

	//alias of select expr -> idx of select expr
	aliasMap map[string]int
	//hash of select expr -> idx of select expr
	projectMap map[string]int

	projectExprs []*Expr
	fromExpr     *Expr
	whereExpr    *Expr
	aggs         []*Expr
	groupbyExprs []*Expr
	orderbyExprs []*Expr
	limitCount   *Expr

	names       []string //output column names
	columnCount int      // count of the select exprs (after expanding star)
}

func NewBuilder() *Builder {
	return &Builder{
		tag:        0,
		rootCtx:    NewBindContext(nil),
		aliasMap:   make(map[string]int),
		projectMap: make(map[string]int),
	}
}

func (b *Builder) Format(ctx *FormatCtx) {
	ctx.Writeln("builder:")
	if b == nil {
		return
	}
	if b.rootCtx != nil {
		ctx.Writeln("bindings:")
		b.rootCtx.Format(ctx)
		ctx.Writeln()
	}
	ctx.Writefln("tag %d", b.tag)
	ctx.Writefln("projectTag %d", b.projectTag)
	ctx.Writefln("groupTag %d", b.groupTag)
	ctx.Writefln("aggTag %d", b.aggTag)

	ctx.Writeln("aliasMap:")
	WriteMap(ctx, b.aliasMap)

	ctx.Writeln("projectMap:")
	WriteMap(ctx, b.projectMap)

	ctx.Writefln("projectExprs:")
	WriteExprs(ctx, b.projectExprs)

	ctx.Writeln("fromExpr:")
	WriteExpr(ctx, b.fromExpr)

	ctx.Writeln("whereExpr:")
	WriteExpr(ctx, b.whereExpr)

	ctx.Writefln("groupbyExprs:")
	WriteExprs(ctx, b.groupbyExprs)

	ctx.Writefln("orderbyExprs:")
	WriteExprs(ctx, b.orderbyExprs)

	ctx.Writeln("limitCount:")
	WriteExpr(ctx, b.limitCount)

	ctx.Writeln("names:")
	ctx.WriteStrings(b.names)

	ctx.Writeln("columnCount:")
	ctx.Writefln("%d", b.columnCount)
}

func (b *Builder) Print(tree treeprint.Tree) {
	if b == nil {
		return
	}
	if b.rootCtx != nil {
		sub := tree.AddBranch("bindings:")
		b.rootCtx.Print(sub)
	}

	tree.AddNode(fmt.Sprintf("tag %d", b.tag))
	tree.AddNode(fmt.Sprintf("projectTag %d", b.projectTag))
	tree.AddNode(fmt.Sprintf("groupTag %d", b.groupTag))
	tree.AddNode(fmt.Sprintf("aggTag %d", b.aggTag))

	sub := tree.AddBranch("aliasMap:")
	WriteMapTree(sub, b.aliasMap)

	sub = tree.AddBranch("projectMap:")
	WriteMapTree(sub, b.projectMap)

	sub = tree.AddBranch("projectExprs:")
	WriteExprsTree(sub, b.projectExprs)

	sub = tree.AddBranch("fromExpr:")
	WriteExprTree(sub, b.fromExpr)

	sub = tree.AddBranch("whereExpr:")
	WriteExprTree(sub, b.whereExpr)

	sub = tree.AddBranch("groupbyExprs:")
	WriteExprsTree(sub, b.groupbyExprs)

	sub = tree.AddBranch("orderbyExprs:")
	WriteExprsTree(sub, b.orderbyExprs)

	sub = tree.AddBranch("limitCount:")
	WriteExprTree(sub, b.limitCount)

	sub = tree.AddBranch("names:")
	sub.AddNode(b.names)

	sub = tree.AddBranch("columnCount:")
	sub.AddNode(fmt.Sprintf("%d", b.columnCount))
}

func (b *Builder) String() string {
	tree := treeprint.NewWithRoot("Builder:")
	b.Print(tree)
	return tree.String()
}

func (b *Builder) GetTag() int {
	b.tag++
	return b.tag
}

func (b *Builder) buildSelect(sel *Ast, ctx *BindContext, depth int) error {
	var err error
	b.projectTag = b.GetTag()
	b.groupTag = b.GetTag()
	b.aggTag = b.GetTag()

	//from
	b.fromExpr, err = b.buildTable(sel.Select.From.Tables, ctx)
	if err != nil {
		return err
	}

	//TODO: expand star

	//select expr alias
	for i, expr := range sel.Select.SelectExprs {
		name := expr.String()
		if expr.Expr.Alias != "" {
			b.aliasMap[expr.Expr.Alias] = i
			name = expr.Expr.Alias
		}
		b.names = append(b.names, name)
		b.projectMap[expr.Hash()] = i
	}
	b.columnCount = len(sel.Select.SelectExprs)

	//where
	if sel.Select.Where.Expr != nil {
		b.whereExpr, err = b.bindExpr(ctx, IWC_WHERE, sel.Select.Where.Expr, depth)
		if err != nil {
			return err
		}
	}

	//order by,limit,distinct
	if len(sel.OrderBy.Exprs) != 0 {
		var retExpr *Expr
		for _, expr := range sel.OrderBy.Exprs {
			retExpr, err = b.bindExpr(ctx, IWC_ORDER, expr, depth)
			if err != nil {
				return err
			}
			b.orderbyExprs = append(b.orderbyExprs, retExpr)
		}
	}

	if sel.Limit.Count != nil {
		b.limitCount, err = b.bindExpr(ctx, IWC_LIMIT, sel.Limit.Count, depth)
		if err != nil {
			return err
		}
	}

	//group by
	if len(sel.Select.GroupBy.Exprs) != 0 {
		var retExpr *Expr
		for _, expr := range sel.Select.GroupBy.Exprs {
			retExpr, err = b.bindExpr(ctx, IWC_GROUP, expr, depth)
			if err != nil {
				return err
			}
			b.groupbyExprs = append(b.groupbyExprs, retExpr)
		}
	}

	//having

	//select exprs
	for _, expr := range sel.Select.SelectExprs {
		var retExpr *Expr
		retExpr, err = b.bindExpr(ctx, IWC_SELECT, expr, depth)
		if err != nil {
			return err
		}
		b.projectExprs = append(b.projectExprs, retExpr)
	}
	return err
}

func (b *Builder) buildFrom(table *Ast, ctx *BindContext) (*Expr, error) {
	return b.buildTable(table, ctx)
}

func (b *Builder) buildTable(table *Ast, ctx *BindContext) (*Expr, error) {
	if table == nil {
		panic("need table")
	}
	switch table.Expr.ExprTyp {
	case AstExprTypeTable:
		{
			db := "tpch"
			table := table.Expr.Svalue
			tpchCatalog := tpchCatalog()
			cta, err := tpchCatalog.Table(db, table)
			if err != nil {
				return nil, err
			}
			b := &Binding{
				typ:     BT_TABLE,
				alias:   table,
				index:   uint64(b.GetTag()),
				typs:    copy(cta.Types),
				names:   copy(cta.Columns),
				nameMap: make(map[string]int),
			}
			for idx, name := range b.names {
				b.nameMap[name] = idx
			}
			err = ctx.AddBinding(table, b)
			if err != nil {
				return nil, err
			}

			return &Expr{
				Typ:       ET_TABLE,
				Index:     b.index,
				Database:  db,
				Table:     table,
				BelongCtx: ctx,
			}, err
		}
	case AstExprTypeJoin:
		return b.buildJoinTable(table, ctx)
	default:
		return nil, fmt.Errorf("usp table type %d", table.Typ)
	}
	return nil, nil
}

func (b *Builder) buildJoinTable(table *Ast, ctx *BindContext) (*Expr, error) {
	leftCtx := NewBindContext(ctx)
	//left
	left, err := b.buildTable(table.Expr.Children[0], leftCtx)
	if err != nil {
		return nil, err
	}

	rightCtx := NewBindContext(ctx)
	//right
	right, err := b.buildTable(table.Expr.Children[1], rightCtx)
	if err != nil {
		return nil, err
	}

	switch table.Expr.JoinTyp {
	case AstJoinTypeCross:
	default:
		return nil, fmt.Errorf("usp join type %d", table.Expr.JoinTyp)
	}

	err = ctx.AddContext(leftCtx)
	if err != nil {
		return nil, err
	}

	err = ctx.AddContext(rightCtx)
	if err != nil {
		return nil, err
	}

	ret := &Expr{
		Typ:       ET_Join,
		BelongCtx: ctx,
		Children:  []*Expr{left, right},
	}

	return ret, err
}

//////////////////////////////////////////////
// create plan
//////////////////////////////////////////////

func (b *Builder) CreatePlan(ctx *BindContext, root *LogicalOperator) (*LogicalOperator, error) {
	var err error
	root, err = b.createFrom(b.fromExpr, root)
	if err != nil {
		return nil, err
	}

	//where
	if b.whereExpr != nil {
		root, err = b.createWhere(b.whereExpr, root)
	}

	//aggregates or group by
	if len(b.aggs) > 0 || len(b.groupbyExprs) > 0 {
		root, err = b.createAggGroup(root)
	}

	//having

	//projects
	if len(b.projectExprs) > 0 {
		root, err = b.createProject(root)
	}

	//order bys
	if len(b.orderbyExprs) > 0 {
		root, err = b.createOrderby(root)
	}

	//limit
	if b.limitCount != nil {
		root = &LogicalOperator{
			Typ:      LOT_Limit,
			Limit:    b.limitCount,
			Children: []*LogicalOperator{root},
		}
	}

	return root, err
}

func (b *Builder) createFrom(expr *Expr, root *LogicalOperator) (*LogicalOperator, error) {
	var err error
	var left, right *LogicalOperator
	switch expr.Typ {
	case ET_TABLE:
		return &LogicalOperator{
			Typ:       LOT_Scan,
			Index:     expr.Index,
			Database:  expr.Database,
			Table:     expr.Table,
			BelongCtx: expr.BelongCtx,
		}, err
	case ET_Join:
		left, err = b.createFrom(expr.Children[0], root)
		if err != nil {
			return nil, err
		}
		right, err = b.createFrom(expr.Children[1], root)
		if err != nil {
			return nil, err
		}
		jt := LOT_JoinTypeCross
		switch expr.JoinTyp {
		case ET_JoinTypeCross, ET_JoinTypeInner:
			jt = LOT_JoinTypeInner
		case ET_JoinTypeLeft:
			jt = LOT_JoinTypeLeft
		default:
			panic(fmt.Sprintf("usp join type %d", jt))
		}
		return &LogicalOperator{
			Typ:      LOT_JOIN,
			JoinTyp:  jt,
			Children: []*LogicalOperator{left, right},
		}, err
	}
	return nil, nil
}

func (b *Builder) createWhere(expr *Expr, root *LogicalOperator) (*LogicalOperator, error) {
	var err error
	var newFilter *Expr

	//TODO:
	//1. find subquery and flatten subquery
	//1. all operators should be changed into (low priority)
	filters := splitExprByAnd(expr)
	var newFilters []*Expr
	for _, filter := range filters {
		newFilter, root, err = b.createSubquery(filter, root)
		if err != nil {
			return nil, err
		}
		newFilters = append(newFilters, newFilter)
	}

	return &LogicalOperator{
		Typ:      LOT_Filter,
		Filters:  newFilters,
		Children: []*LogicalOperator{root},
	}, err
}

// if the expr has subquery, it flattens the subquery and replaces
// the expr.
func (b *Builder) createSubquery(expr *Expr, root *LogicalOperator) (*Expr, *LogicalOperator, error) {
	var err error
	var subRoot *LogicalOperator
	switch expr.Typ {
	case ET_Subquery:
		subBuilder := expr.SubBuilder
		subCtx := expr.SubCtx
		subRoot, err = subBuilder.CreatePlan(subCtx, nil)
		if err != nil {
			return nil, nil, err
		}
		//flatten subquery
		return b.apply(expr, root, subRoot)
	case ET_Equal, ET_And, ET_Like:
		left, lroot, err := b.createSubquery(expr.Children[0], root)
		if err != nil {
			return nil, nil, err
		}
		right, rroot, err := b.createSubquery(expr.Children[1], lroot)
		if err != nil {
			return nil, nil, err
		}
		return &Expr{
			Typ:      expr.Typ,
			DataTyp:  expr.DataTyp,
			Children: []*Expr{left, right},
		}, rroot, nil
	case ET_Func:
		var childExpr *Expr
		args := make([]*Expr, 0)
		for _, child := range expr.Children {
			childExpr, root, err = b.createSubquery(child, root)
			if err != nil {
				return nil, nil, err
			}
			args = append(args, childExpr)
		}
		return &Expr{
			Typ:      expr.Typ,
			Svalue:   expr.Svalue,
			FuncId:   expr.FuncId,
			DataTyp:  expr.DataTyp,
			Children: args,
		}, root, nil
	case ET_Column:
		return expr, root, nil
	case ET_IConst, ET_SConst:
		return expr, root, nil
	default:
		panic(fmt.Sprintf("usp %v", expr.Typ))
	}
	return nil, nil, err
}

// apply flattens subquery
// Based On Paper: Orthogonal Optimization of Subqueries and Aggregation
// make APPLY(expr,root,subRoot) algorithm
// expr: subquery expr
// root: root of the query that subquery belongs to
// subquery: root of the subquery
func (b *Builder) apply(expr *Expr, root, subRoot *LogicalOperator) (*Expr, *LogicalOperator, error) {
	if expr.Typ != ET_Subquery {
		panic("must be subquery")
	}
	corrExprs := collectCorrFilter(subRoot)
	//collect correlated columns
	corrCols := make([]*Expr, 0)
	for _, corr := range corrExprs {
		corrCols = append(corrCols, collectCorrColumn(corr)...)
	}
	if len(corrExprs) > 0 {
		//correlated subquery
		newSub, err := b.applyImpl(corrExprs, corrCols, root, subRoot)
		if err != nil {
			return nil, nil, err
		}

		//TODO: may have multi columns
		subBuilder := expr.SubBuilder
		proj0 := subBuilder.projectExprs[0]
		colRef := &Expr{
			Typ:     ET_Column,
			DataTyp: proj0.DataTyp,
			Table:   proj0.Table,
			Name:    proj0.Name,
			ColRef: [2]uint64{
				uint64(subBuilder.projectTag),
				0,
			},
		}

		return colRef, newSub, nil
	} else {
		newRoot := &LogicalOperator{
			Typ:     LOT_JOIN,
			JoinTyp: LOT_JoinTypeInner,
			OnConds: nil,
			Children: []*LogicalOperator{
				root, subRoot,
			},
		}
		// TODO: may have multi columns
		subBuilder := expr.SubBuilder
		proj0 := subBuilder.projectExprs[0]
		colRef := &Expr{
			Typ:     ET_Column,
			DataTyp: proj0.DataTyp,
			Table:   proj0.Table,
			Name:    proj0.Name,
			ColRef: [2]uint64{
				uint64(subBuilder.projectTag),
				0,
			},
		}
		return colRef, newRoot, nil
	}
}

// TODO: wrong impl.
// need add function: check LogicalOperator has cor column
func (b *Builder) applyImpl(corrExprs []*Expr, corrCols []*Expr, root, subRoot *LogicalOperator) (*LogicalOperator, error) {
	var err error
	has := hasCorrColInRoot(subRoot)
	if !has {
		//remove cor column
		nonCorrExprs, newCorrExprs := removeCorrExprs(corrExprs)

		//TODO: wrong!!!
		newRoot := &LogicalOperator{
			Typ:     LOT_JOIN,
			JoinTyp: LOT_JoinTypeInner,
			OnConds: nonCorrExprs,
			Children: []*LogicalOperator{
				root, subRoot,
			},
		}

		if len(newCorrExprs) > 0 {
			newRoot = &LogicalOperator{
				Typ:      LOT_Filter,
				Filters:  newCorrExprs,
				Children: []*LogicalOperator{newRoot},
			}
		}
		return newRoot, err
	}
	switch subRoot.Typ {
	case LOT_Project:
		for _, proj := range subRoot.Projects {
			if hasCorrCol(proj) {
				panic("usp correlated column in project")
			}
		}
		if has {
			subRoot.Projects = append(subRoot.Projects, corrCols...)
		}
		subRoot.Children[0], err = b.applyImpl(corrExprs, corrCols, root, subRoot.Children[0])
		return subRoot, err
	case LOT_AggGroup:
		for _, by := range subRoot.GroupBys {
			if hasCorrCol(by) {
				panic("usp correlated column in agg")
			}
		}
		if has {
			subRoot.GroupBys = append(subRoot.GroupBys, corrCols...)
		}
		subRoot.Children[0], err = b.applyImpl(corrExprs, corrCols, root, subRoot.Children[0])
		return subRoot, err
	case LOT_Filter:
		filterHasCorrCol := false
		for _, filter := range subRoot.Filters {
			if hasCorrCol(filter) {
				filterHasCorrCol = true
				break
			}
		}
		if has && !filterHasCorrCol {
			subRoot.Filters = append(subRoot.Filters, corrExprs...)
		}
		subRoot.Children[0], err = b.applyImpl(corrExprs, corrCols, root, subRoot.Children[0])
		return subRoot, err
	}
	return subRoot, nil
}

func (b *Builder) createAggGroup(root *LogicalOperator) (*LogicalOperator, error) {
	return &LogicalOperator{
		Typ:      LOT_AggGroup,
		Index:    uint64(b.groupTag),
		Index2:   uint64(b.aggTag),
		Aggs:     b.aggs,
		GroupBys: b.groupbyExprs,
		Children: []*LogicalOperator{root},
	}, nil
}

func (b *Builder) createProject(root *LogicalOperator) (*LogicalOperator, error) {
	var err error
	var newExpr *Expr
	projects := make([]*Expr, 0)
	for _, expr := range b.projectExprs {
		newExpr, root, err = b.createSubquery(expr, root)
		if err != nil {
			return nil, err
		}
		projects = append(projects, newExpr)
	}
	return &LogicalOperator{
		Typ:      LOT_Project,
		Index:    uint64(b.projectTag),
		Projects: projects,
		Children: []*LogicalOperator{root},
	}, nil
}

func (b *Builder) createOrderby(root *LogicalOperator) (*LogicalOperator, error) {
	return &LogicalOperator{
		Typ:      LOT_Order,
		OrderBys: b.orderbyExprs,
		Children: []*LogicalOperator{root},
	}, nil
}

// collectCorrFilter collects all exprs that has correlated column.
// and does not remove these exprs.
func collectCorrFilter(root *LogicalOperator) []*Expr {
	var ret, childRet []*Expr
	for _, child := range root.Children {
		childRet = collectCorrFilter(child)
		ret = append(ret, childRet...)
	}

	switch root.Typ {
	case LOT_Filter:
		for _, filter := range root.Filters {
			if hasCorrCol(filter) {
				ret = append(ret, filter)
			}
		}
	default:
	}
	return ret
}

// collectCorrColumn collects all correlated columns
func collectCorrColumn(expr *Expr) []*Expr {
	var ret []*Expr
	switch expr.Typ {
	case ET_Column:
		if expr.Depth > 0 {
			return []*Expr{expr}
		}
	case ET_Equal, ET_And, ET_Like:
		ret = append(ret, collectCorrColumn(expr.Children[0])...)
		ret = append(ret, collectCorrColumn(expr.Children[1])...)
	case ET_Func:
		for _, child := range expr.Children {
			ret = append(ret, collectCorrColumn(child)...)
		}
	default:
		panic(fmt.Sprintf("usp %v", expr.Typ))
	}
	return ret
}

func hasCorrCol(expr *Expr) bool {
	switch expr.Typ {
	case ET_Column:
		return expr.Depth > 0
	case ET_Equal, ET_And, ET_Like:
		return hasCorrCol(expr.Children[0]) || hasCorrCol(expr.Children[1])
	case ET_Func:
		for _, child := range expr.Children {
			if hasCorrCol(child) {
				return true
			}
		}
		return false
	case ET_IConst, ET_SConst:
		return false
	default:
		panic(fmt.Sprintf("usp %v", expr.Typ))
	}
	return false
}

// hasCorrColInRoot checks if the plan with root has the correlated column.
func hasCorrColInRoot(root *LogicalOperator) bool {
	switch root.Typ {
	case LOT_Project:
		for _, proj := range root.Projects {
			if hasCorrCol(proj) {
				panic("usp correlated column in project")
			}
		}

	case LOT_AggGroup:
		for _, by := range root.GroupBys {
			if hasCorrCol(by) {
				panic("usp correlated column in agg")
			}
		}

	case LOT_Filter:
		for _, filter := range root.Filters {
			if hasCorrCol(filter) {
				return true
			}
		}
	}

	for _, child := range root.Children {
		if hasCorrColInRoot(child) {
			return true
		}
	}
	return false
}

// ==============
// Optimize plan
// ==============
func (b *Builder) Optimize(ctx *BindContext, root *LogicalOperator) (*LogicalOperator, error) {
	var err error
	var left []*Expr
	//1. pushdown filter
	root, left, err = b.pushdownFilters(root, nil)
	if err != nil {
		return nil, err
	}
	if len(left) > 0 {
		root = &LogicalOperator{
			Typ:      LOT_Filter,
			Filters:  left,
			Children: []*LogicalOperator{root},
		}
	}

	//2. join order
	root, err = b.joinOrder(root)
	if err != nil {
		return nil, err
	}

	//3. column prune
	return root, nil
}

// pushdownFilters pushes down filters to the lowest possible position.
// It returns the new root and the filters that cannot be pushed down.
func (b *Builder) pushdownFilters(root *LogicalOperator, filters []*Expr) (*LogicalOperator, []*Expr, error) {
	var err error
	var left, childLeft []*Expr
	var childRoot *LogicalOperator
	var needs []*Expr

	switch root.Typ {
	case LOT_Scan:
		for _, f := range filters {
			if onlyReferTo(f, root.Index) {
				//expr that only refer to the scan expr can be pushdown.
				root.Filters = append(root.Filters, f)
			} else {
				left = append(left, f)
			}
		}
	case LOT_JOIN:
		needs = filters
		leftTags := make(map[uint64]bool)
		rightTags := make(map[uint64]bool)
		collectTags(root.Children[0], leftTags)
		collectTags(root.Children[1], rightTags)

		root.OnConds = splitExprsByAnd(root.OnConds)
		if root.JoinTyp == LOT_JoinTypeInner {
			for _, on := range root.OnConds {
				needs = append(needs, splitExprByAnd(on)...)
			}
			root.OnConds = nil
		}

		whichSides := make([]int, len(needs))
		for i, nd := range needs {
			whichSides[i] = decideSide(nd, leftTags, rightTags)
		}

		leftNeeds := make([]*Expr, 0)
		rightNeeds := make([]*Expr, 0)
		for i, nd := range needs {
			switch whichSides[i] {
			case NoneSide:
				switch root.JoinTyp {
				case LOT_JoinTypeInner:
					leftNeeds = append(leftNeeds, copyExpr(nd))
					rightNeeds = append(rightNeeds, nd)
				case LOT_JoinTypeLeft:
					leftNeeds = append(leftNeeds, nd)
				default:
					left = append(left, nd)
				}
			case LeftSide:
				leftNeeds = append(leftNeeds, nd)
			case RightSide:
				rightNeeds = append(rightNeeds, nd)
			case BothSide:
				if root.JoinTyp == LOT_JoinTypeInner {
					root.OnConds = append(root.OnConds, nd)
					break
				}
				left = append(left, nd)
			default:
				panic(fmt.Sprintf("usp side %d", whichSides[i]))
			}
		}

		childRoot, childLeft, err = b.pushdownFilters(root.Children[0], leftNeeds)
		if err != nil {
			return nil, nil, err
		}
		if len(childLeft) > 0 {
			childRoot = &LogicalOperator{
				Typ:      LOT_Filter,
				Filters:  childLeft,
				Children: []*LogicalOperator{childRoot},
			}
		}
		root.Children[0] = childRoot

		childRoot, childLeft, err = b.pushdownFilters(root.Children[1], rightNeeds)
		if err != nil {
			return nil, nil, err
		}
		if len(childLeft) > 0 {
			childRoot = &LogicalOperator{
				Typ:      LOT_Filter,
				Filters:  childLeft,
				Children: []*LogicalOperator{childRoot},
			}
		}
		root.Children[1] = childRoot

	case LOT_AggGroup:
		for _, f := range filters {
			if referTo(f, root.Index2) {
				//expr that refer to the agg exprs can not be pushdown.
				root.Filters = append(root.Filters, f)
			} else {
				//restore the real expr for the expr that refer to the expr in the group by.
				needs = append(needs, restoreExpr(f, root.Index, root.GroupBys))
			}
		}

		childRoot, childLeft, err = b.pushdownFilters(root.Children[0], needs)
		if err != nil {
			return nil, nil, err
		}
		if len(childLeft) > 0 {
			childRoot = &LogicalOperator{
				Typ:      LOT_Filter,
				Filters:  childLeft,
				Children: []*LogicalOperator{childRoot},
			}
		}
		root.Children[0] = childRoot
	case LOT_Project:
		//restore the real expr for the expr that refer to the expr in the project list.
		for _, f := range filters {
			needs = append(needs, restoreExpr(f, root.Index, root.Projects))
		}

		childRoot, childLeft, err = b.pushdownFilters(root.Children[0], needs)
		if err != nil {
			return nil, nil, err
		}
		if len(childLeft) > 0 {
			childRoot = &LogicalOperator{
				Typ:      LOT_Filter,
				Filters:  childLeft,
				Children: []*LogicalOperator{childRoot},
			}
		}
		root.Children[0] = childRoot
	case LOT_Filter:
		needs = filters
		for _, e := range root.Filters {
			needs = append(needs, splitExprByAnd(e)...)
		}
		childRoot, childLeft, err = b.pushdownFilters(root.Children[0], needs)
		if err != nil {
			return nil, nil, err
		}
		if len(childLeft) > 0 {
			root.Children[0] = childRoot
			root.Filters = childLeft
		} else {
			//remove this FILTER node
			root = childRoot
		}

	default:
		if root.Typ == LOT_Limit {
			//can not pushdown filter through LIMIT
			left, filters = filters, nil
		}
		if len(root.Children) > 0 {
			if len(root.Children) > 1 {
				panic("must be on child: " + root.Typ.String())
			}
			childRoot, childLeft, err = b.pushdownFilters(root.Children[0], filters)
			if err != nil {
				return nil, nil, err
			}
			if len(childLeft) > 0 {
				childRoot = &LogicalOperator{
					Typ:      LOT_Filter,
					Filters:  childLeft,
					Children: []*LogicalOperator{childRoot},
				}
			}
			root.Children[0] = childRoot
		} else {
			left = filters
		}
	}

	return root, left, err
}

func collectTags(root *LogicalOperator, set map[uint64]bool) {
	if root.Index != 0 {
		set[root.Index] = true
	}
	if root.Index2 != 0 {
		set[root.Index2] = true
	}
	for _, child := range root.Children {
		collectTags(child, set)
	}
}

const (
	NoneSide       = 0
	LeftSide       = 1 << 1
	RightSide      = 1 << 2
	BothSide       = LeftSide | RightSide
	CorrelatedSide = 1 << 3
)
