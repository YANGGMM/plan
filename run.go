package main

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	pqLocal "github.com/xitongsys/parquet-go-source/local"
	pqReader "github.com/xitongsys/parquet-go/reader"
	"github.com/xitongsys/parquet-go/source"
)

type OperatorState struct {
	//order
	orderKeyExec *ExprExec
	keyTypes     []LType
	payloadTypes []LType

	projTypes  []LType
	projExec   *ExprExec
	outputExec *ExprExec

	//filter projExec used in aggr, filter, scan
	filterExec *ExprExec
	filterSel  *SelectVector

	//for aggregate
	haScanState           *HashAggrScanState
	groupbyWithParamsExec *ExprExec
	groupbyExec           *ExprExec

	showRaw bool
}

type OperatorResult int

const (
	InvalidOpResult OperatorResult = 0
	NeedMoreInput   OperatorResult = 1
	haveMoreOutput  OperatorResult = 2
	Done            OperatorResult = 3
)

var _ OperatorExec = &Runner{}

type OperatorExec interface {
	Init() error
	Execute(input, output *Chunk, state *OperatorState) (OperatorResult, error)
	Close() error
}

type Runner struct {
	op    *PhysicalOperator
	state *OperatorState
	//for order
	localSort *LocalSort

	//for hash aggr
	hAggr *HashAggr

	//for hash join
	hjoin    *HashJoin
	joinKeys *Chunk

	//for scan
	pqFile        source.ParquetFile
	pqReader      *pqReader.ParquetReader
	dataFile      *os.File
	reader        *csv.Reader
	colIndice     []int
	readedColTyps []LType
	tablePath     string

	//common
	outputTypes  []LType
	outputIndice []int
	children     []*Runner
}

func (run *Runner) initChildren() error {
	run.children = []*Runner{}
	for _, child := range run.op.Children {
		childRun := &Runner{
			op:    child,
			state: &OperatorState{},
		}
		err := childRun.Init()
		if err != nil {
			return err
		}
		run.children = append(run.children, childRun)
	}
	return nil
}

func (run *Runner) Init() error {
	for _, output := range run.op.Outputs {
		run.outputTypes = append(run.outputTypes, output.DataTyp.LTyp)
		run.outputIndice = append(run.outputIndice, int(output.ColRef.column()))
	}
	err := run.initChildren()
	if err != nil {
		return err
	}
	switch run.op.Typ {
	case POT_Scan:
		return run.scanInit()
	case POT_Project:
		return run.projInit()
	case POT_Join:
		return run.joinInit()
	case POT_Agg:
		return run.aggrInit()
	case POT_Filter:
		return run.filterInit()
	case POT_Order:
		return run.orderInit()
	default:
		panic("usp")
	}
	return nil
}

func (run *Runner) Execute(input, output *Chunk, state *OperatorState) (OperatorResult, error) {
	output.init(run.outputTypes, defaultVectorSize)
	switch run.op.Typ {
	case POT_Scan:
		return run.scanExec(output, state)
	case POT_Project:
		return run.projExec(output, state)
	case POT_Join:
		return run.joinExec(output, state)
	case POT_Agg:
		return run.aggrExec(output, state)
	case POT_Filter:
		return run.filterExec(output, state)
	case POT_Order:
		return run.orderExec(output, state)
	default:
		panic("usp")
	}
	return Done, nil
}

func (run *Runner) execChild(child *Runner, output *Chunk, state *OperatorState) (OperatorResult, error) {
	cnt := 0
	for output.card() == 0 {
		res, err := child.Execute(nil, output, child.state)
		if err != nil {
			return InvalidOpResult, err
		}
		//fmt.Println("child result:", res, cnt)
		cnt++
		switch res {
		case Done:
			return Done, nil
		case InvalidOpResult:
			return InvalidOpResult, nil
		default:
			return haveMoreOutput, nil
		}
	}
	return Done, nil
}

func (run *Runner) Close() error {
	for _, child := range run.children {
		err := child.Close()
		if err != nil {
			return err
		}
	}
	switch run.op.Typ {
	case POT_Scan:
		return run.scanClose()
	case POT_Project:
		return run.projClose()
	case POT_Join:
		return run.joinClose()
	case POT_Agg:
		return run.aggrClose()
	case POT_Filter:
		return run.filterClose()
	case POT_Order:
		return run.orderClose()
	default:
		panic("usp")
	}
	return nil
}

func (run *Runner) orderInit() error {
	//TODO: asc or desc
	keyTypes := make([]LType, 0)
	realOrderByExprs := make([]*Expr, 0)
	for _, by := range run.op.OrderBys {
		child := by.Children[0]
		keyTypes = append(keyTypes, child.DataTyp.LTyp)
		realOrderByExprs = append(realOrderByExprs, child)
	}

	payLoadTypes := make([]LType, 0)
	for _, output := range run.op.Outputs {
		payLoadTypes = append(payLoadTypes,
			output.DataTyp.LTyp)
	}

	run.localSort = NewLocalSort(
		NewSortLayout(run.op.OrderBys),
		NewRowLayout(payLoadTypes, nil),
	)

	run.state = &OperatorState{
		keyTypes:     keyTypes,
		payloadTypes: payLoadTypes,
		orderKeyExec: NewExprExec(realOrderByExprs...),
		outputExec:   NewExprExec(run.op.Outputs...),
	}

	return nil
}

func (run *Runner) orderExec(output *Chunk, state *OperatorState) (OperatorResult, error) {
	var err error
	var res OperatorResult
	if run.localSort._sortState == SS_INIT {
		cnt := 0
		for {
			childChunk := &Chunk{}
			res, err = run.execChild(run.children[0], childChunk, state)
			if err != nil {
				return 0, err
			}
			if res == InvalidOpResult {
				return InvalidOpResult, nil
			}
			if res == Done {
				break
			}
			if childChunk.card() == 0 {
				continue
			}

			//fmt.Println("childChunk:")
			//childChunk.print()

			//evaluate order by expr
			key := &Chunk{}
			key.init(run.state.keyTypes, defaultVectorSize)
			err = run.state.orderKeyExec.executeExprs(
				[]*Chunk{childChunk, nil, nil},
				key,
			)
			if err != nil {
				return 0, err
			}

			//fmt.Println("key1:")
			//key.print()

			//evaluate payload expr
			payload := &Chunk{}
			payload.init(run.state.payloadTypes, defaultVectorSize)

			err = run.state.outputExec.executeExprs(
				[]*Chunk{childChunk, nil, nil},
				payload,
			)
			if err != nil {
				return 0, err
			}

			assertFunc(key.card() != 0 && payload.card() != 0)
			cnt += key.card()
			assertFunc(key.card() == payload.card())
			//fmt.Println("key2:")
			//key.print()
			//fmt.Println("payload:")
			//payload.print()

			run.localSort.SinkChunk(key, payload)
		}
		fmt.Println("total count", cnt)
		run.localSort._sortState = SS_SORT
	}

	if run.localSort._sortState == SS_SORT {
		//get all chunks from child
		run.localSort.Sort(true)
		run.localSort._sortState = SS_SCAN
	}

	if run.localSort._sortState == SS_SCAN {
		if run.localSort._scanner != nil &&
			run.localSort._scanner.Remaining() == 0 {
			run.localSort._scanner = nil
		}

		if run.localSort._scanner == nil {
			run.localSort._scanner = NewPayloadScanner(
				run.localSort._sortedBlocks[0]._payloadData,
				run.localSort,
				true,
			)
		}

		run.localSort._scanner.Scan(output)
	}

	if output.card() == 0 {
		return Done, nil
	}
	return haveMoreOutput, nil
}

func (run *Runner) orderClose() error {
	run.localSort = nil
	return nil
}

func (run *Runner) filterInit() error {
	var err error
	var filterExec *ExprExec
	filterExec, err = initFilterExec(run.op.Filters)
	if err != nil {
		return err
	}
	run.state = &OperatorState{
		filterExec: filterExec,
		filterSel:  NewSelectVector(defaultVectorSize),
	}
	return nil
}

func initFilterExec(filters []*Expr) (*ExprExec, error) {
	//init filter
	//convert filters into "... AND ..."
	var err error
	var andFilter *Expr
	if len(filters) > 0 {
		var impl *Impl
		andFilter = filters[0]
		for i, filter := range filters {
			if i > 0 {
				if andFilter.DataTyp.LTyp.id != LTID_BOOLEAN ||
					filter.DataTyp.LTyp.id != LTID_BOOLEAN {
					return nil, fmt.Errorf("need boolean expr")
				}
				argsTypes := []ExprDataType{
					andFilter.DataTyp,
					filter.DataTyp,
				}
				impl, err = GetFunctionImpl(
					AND,
					argsTypes)
				if err != nil {
					return nil, err
				}
				andFilter = &Expr{
					Typ:     ET_Func,
					SubTyp:  ET_And,
					DataTyp: impl.RetTypeDecider(argsTypes),
					FuncId:  AND,
					Children: []*Expr{
						andFilter,
						filter,
					},
				}
			}
		}
	}
	return NewExprExec(andFilter), nil
}

func (run *Runner) runFilterExec(input *Chunk, output *Chunk, filterOnLocal bool) error {
	//filter
	var err error
	var count int
	if filterOnLocal {
		count, err = run.state.filterExec.executeSelect([]*Chunk{nil, nil, input}, run.state.filterSel)
		if err != nil {
			return err
		}
	} else {
		count, err = run.state.filterExec.executeSelect([]*Chunk{input, nil, nil}, run.state.filterSel)
		if err != nil {
			return err
		}
	}

	if count == input.card() {
		//reference
		output.referenceIndice(input, run.outputIndice)
	} else {
		//slice
		output.sliceIndice(input, run.state.filterSel, count, 0, run.outputIndice)
	}
	return nil
}

func (run *Runner) filterExec(output *Chunk, state *OperatorState) (OperatorResult, error) {
	childChunk := &Chunk{}
	var res OperatorResult
	var err error
	if len(run.children) != 0 {
		res, err = run.execChild(run.children[0], childChunk, state)
		if err != nil {
			return 0, err
		}
		if res == InvalidOpResult {
			return InvalidOpResult, nil
		}
		if res == Done {
			return res, nil
		}
	}

	err = run.runFilterExec(childChunk, output, false)
	if err != nil {
		return 0, err
	}
	return haveMoreOutput, nil
}

func (run *Runner) filterClose() error {
	return nil
}

func (run *Runner) aggrInit() error {
	run.state = &OperatorState{}
	if len(run.op.GroupBys) == 0 /*&& groupingSet*/ {
		run.hAggr = NewHashAggr(
			run.outputTypes,
			run.op.Aggs,
			nil,
			nil,
			nil,
		)
	} else {
		run.hAggr = NewHashAggr(
			run.outputTypes,
			run.op.Aggs,
			run.op.GroupBys,
			nil,
			nil,
		)
		//groupby exprs + param exprs of aggr functions
		groupExprs := make([]*Expr, 0)
		groupExprs = append(groupExprs, run.hAggr._groupedAggrData._groups...)
		groupExprs = append(groupExprs, run.hAggr._groupedAggrData._paramExprs...)
		run.state.groupbyWithParamsExec = NewExprExec(groupExprs...)
		run.state.groupbyExec = NewExprExec(run.hAggr._groupedAggrData._groups...)
		run.state.filterExec = NewExprExec(run.op.Filters...)
		run.state.filterSel = NewSelectVector(defaultVectorSize)
		run.state.outputExec = NewExprExec(run.op.Outputs...)
	}
	return nil
}

func (run *Runner) aggrExec(output *Chunk, state *OperatorState) (OperatorResult, error) {
	var err error
	var res OperatorResult
	if run.hAggr._has == HAS_INIT {
		cnt := 0
		for {
			childChunk := &Chunk{}
			res, err = run.execChild(run.children[0], childChunk, state)
			if err != nil {
				return 0, err
			}
			if res == InvalidOpResult {
				return InvalidOpResult, nil
			}
			if res == Done {
				break
			}
			if childChunk.card() == 0 {
				continue
			}
			//fmt.Println("build aggr", cnt, childChunk.card())
			//childChunk.print()
			cnt += childChunk.card()

			typs := make([]LType, 0)
			typs = append(typs, run.hAggr._groupedAggrData._groupTypes...)
			typs = append(typs, run.hAggr._groupedAggrData._payloadTypes...)
			groupChunk := &Chunk{}
			groupChunk.init(typs, defaultVectorSize)
			err = run.state.groupbyWithParamsExec.executeExprs([]*Chunk{childChunk, nil, nil}, groupChunk)
			if err != nil {
				return InvalidOpResult, err
			}
			run.hAggr.Sink(groupChunk)
		}
		run.hAggr._has = HAS_SCAN
		fmt.Println("child cnt", cnt)
	}
	if run.hAggr._has == HAS_SCAN {
		if run.state.haScanState == nil {
			run.state.haScanState = NewHashAggrScanState()
			err = run.initChildren()
			if err != nil {
				return InvalidOpResult, err
			}
		}

		for {
			//1.get child chunk from children[0]
			childChunk := &Chunk{}
			res, err = run.execChild(run.children[0], childChunk, state)
			if err != nil {
				return 0, err
			}
			if res == InvalidOpResult {
				return InvalidOpResult, nil
			}
			if res == Done {
				break
			}
			if childChunk.card() == 0 {
				continue
			}

			//fmt.Println("scan aggr", childChunk.card())
			//childChunk.print()
			x := childChunk.card()
			run.state.haScanState._childCnt += childChunk.card()

			//2.eval the group by exprs for the child chunk
			typs := make([]LType, 0)
			typs = append(typs, run.hAggr._groupedAggrData._groupTypes...)
			groupChunk := &Chunk{}
			groupChunk.init(typs, defaultVectorSize)
			err = run.state.groupbyExec.executeExprs([]*Chunk{childChunk, nil, nil}, groupChunk)
			if err != nil {
				return InvalidOpResult, err
			}

			//3.get aggr states for the group
			aggrStatesChunk := &Chunk{}
			aggrStatesTyps := make([]LType, 0)
			aggrStatesTyps = append(aggrStatesTyps, run.hAggr._groupedAggrData._aggrReturnTypes...)
			aggrStatesChunk.init(aggrStatesTyps, defaultVectorSize)
			res = run.hAggr.FetechAggregates(run.state.haScanState, groupChunk, aggrStatesChunk)
			if res == InvalidOpResult {
				return InvalidOpResult, nil
			}
			if res == Done {
				return res, nil
			}

			//4.eval the filter on (child chunk + aggr states)
			//fmt.Println("=============")
			//childChunk.print()
			//aggrStatesChunk.print()
			var count int
			count, err = state.filterExec.executeSelect([]*Chunk{childChunk, nil, aggrStatesChunk}, state.filterSel)
			if err != nil {
				return InvalidOpResult, err
			}

			if count == 0 {
				run.state.haScanState._filteredCnt1 += childChunk.card() - count
				continue
			}

			var childChunk2 *Chunk
			var aggrStatesChunk2 *Chunk
			var filtered int
			if count == childChunk.card() {
				childChunk2 = childChunk
				aggrStatesChunk2 = aggrStatesChunk
			} else {
				filtered = count - childChunk.card()
				run.state.haScanState._filteredCnt2 += filtered

				childChunkIndice := make([]int, 0)
				for i := 0; i < childChunk.columnCount(); i++ {
					childChunkIndice = append(childChunkIndice, i)
				}
				aggrStatesChunkIndice := make([]int, 0)
				for i := 0; i < aggrStatesChunk.columnCount(); i++ {
					aggrStatesChunkIndice = append(aggrStatesChunkIndice, i)
				}
				childChunk2 = &Chunk{}
				childChunk2.init(run.children[0].outputTypes, defaultVectorSize)
				aggrStatesChunk2 = &Chunk{}
				aggrStatesChunk2.init(aggrStatesTyps, defaultVectorSize)

				//slice
				childChunk2.sliceIndice(childChunk, state.filterSel, count, 0, childChunkIndice)
				aggrStatesChunk2.sliceIndice(aggrStatesChunk, state.filterSel, count, 0, aggrStatesChunkIndice)

			}
			assertFunc(childChunk.card() == childChunk2.card())
			assertFunc(aggrStatesChunk.card() == aggrStatesChunk2.card())
			assertFunc(childChunk2.card() == aggrStatesChunk2.card())

			//5. eval the output
			err = run.state.outputExec.executeExprs([]*Chunk{childChunk2, nil, aggrStatesChunk2}, output)
			if err != nil {
				return InvalidOpResult, err
			}
			if filtered == 0 {
				assertFunc(filtered == 0)
				assertFunc(output.card() == childChunk2.card())
				assertFunc(x >= childChunk2.card())
			}
			assertFunc(output.card()+filtered == childChunk2.card())
			assertFunc(x == childChunk.card())
			assertFunc(output.card() == childChunk.card())

			run.state.haScanState._outputCnt += output.card()
			run.state.haScanState._childCnt2 += childChunk.card()
			run.state.haScanState._childCnt3 += x
			if output.card() > 0 {
				return haveMoreOutput, nil
			}
		}
	}
	fmt.Println("scan cnt",
		"childCnt",
		run.state.haScanState._childCnt,
		"childCnt2",
		run.state.haScanState._childCnt2,
		"childCnt3",
		run.state.haScanState._childCnt3,
		"outputCnt",
		run.state.haScanState._outputCnt,
		"filteredCnt1",
		run.state.haScanState._filteredCnt1,
		"filteredCnt2",
		run.state.haScanState._filteredCnt2,
	)
	return Done, nil
}

func (run *Runner) aggrClose() error {
	run.hAggr = nil
	return nil
}

func (run *Runner) joinInit() error {
	run.hjoin = NewHashJoin(run.op, run.op.OnConds)
	run.state = &OperatorState{
		outputExec: NewExprExec(run.op.Outputs...),
	}
	return nil
}

func (run *Runner) joinExec(output *Chunk, state *OperatorState) (OperatorResult, error) {
	//1. Build Hash Table on the right child
	res, err := run.joinBuildHashTable(state)
	if err != nil {
		return InvalidOpResult, err
	}
	if res == InvalidOpResult {
		return InvalidOpResult, nil
	}
	//2. probe stage
	//probe
	if run.hjoin._hjs == HJS_BUILD || run.hjoin._hjs == HJS_PROBE {
		if run.hjoin._hjs == HJS_BUILD {
			run.hjoin._hjs = HJS_PROBE
		}

		//continue unfinished can
		if run.hjoin._scan != nil {
			nextChunk := Chunk{}
			nextChunk.init(run.hjoin._scanNextTyps, defaultVectorSize)
			run.hjoin._scan.Next(run.hjoin._joinKeys, run.hjoin._scan._leftChunk, &nextChunk)
			if nextChunk.card() > 0 {
				err = run.evalJoinOutput(&nextChunk, output)
				if err != nil {
					return 0, err
				}
				return haveMoreOutput, nil
			}
			run.hjoin._scan = nil
		}

		//probe
		leftChunk := &Chunk{}
		res, err = run.execChild(run.children[0], leftChunk, state)
		if err != nil {
			return 0, err
		}
		switch res {
		case Done:
			return Done, nil
		case InvalidOpResult:
			return InvalidOpResult, nil
		}

		run.hjoin._joinKeys.reset()
		err = run.hjoin._probExec.executeExprs([]*Chunk{leftChunk, nil, nil}, run.hjoin._joinKeys)
		if err != nil {
			return 0, err
		}
		run.hjoin._scan = run.hjoin._ht.Probe(run.hjoin._joinKeys)
		run.hjoin._scan._leftChunk = leftChunk
		nextChunk := Chunk{}
		nextChunk.init(run.hjoin._scanNextTyps, defaultVectorSize)
		run.hjoin._scan.Next(run.hjoin._joinKeys, run.hjoin._scan._leftChunk, &nextChunk)
		if nextChunk.card() > 0 {
			err = run.evalJoinOutput(&nextChunk, output)
			if err != nil {
				return 0, err
			}
			return haveMoreOutput, nil
		} else {
			run.hjoin._scan = nil
		}
		return haveMoreOutput, nil
	}
	return 0, nil
}

func (run *Runner) evalJoinOutput(nextChunk, output *Chunk) (err error) {
	leftChunk := Chunk{}
	leftTyps := run.hjoin._scanNextTyps[:len(run.hjoin._leftIndice)]
	leftChunk.init(leftTyps, defaultVectorSize)
	leftChunk.referenceIndice(nextChunk, run.hjoin._leftIndice)

	rightChunk := Chunk{}
	rightChunk.init(run.hjoin._buildTypes, defaultVectorSize)
	rightChunk.referenceIndice(nextChunk, run.hjoin._rightIndice)
	err = run.state.outputExec.executeExprs(
		[]*Chunk{
			&leftChunk,
			&rightChunk,
			nil,
		},
		output,
	)
	return err
}

func (run *Runner) joinBuildHashTable(state *OperatorState) (OperatorResult, error) {
	var err error
	var res OperatorResult
	if run.hjoin._hjs == HJS_INIT {
		run.hjoin._hjs = HJS_BUILD
		cnt := 0
		for {
			rightChunk := &Chunk{}
			res, err = run.execChild(run.children[1], rightChunk, state)
			if err != nil {
				return 0, err
			}
			if res == InvalidOpResult {
				return InvalidOpResult, nil
			}
			if res == Done {
				run.hjoin._ht.Finalize()
				break
			}
			//fmt.Println("build hash table", cnt)
			cnt++
			err = run.hjoin.Build(rightChunk)
			if err != nil {
				return 0, err
			}
		}
		run.hjoin._hjs = HJS_PROBE
	}

	return Done, nil
}

func (run *Runner) joinClose() error {
	run.hjoin = nil
	return nil
}

func (run *Runner) projInit() error {
	projTypes := make([]LType, 0)
	for _, proj := range run.op.Projects {
		projTypes = append(projTypes, proj.DataTyp.LTyp)
	}
	run.state = &OperatorState{
		projTypes:  projTypes,
		projExec:   NewExprExec(run.op.Projects...),
		outputExec: NewExprExec(run.op.Outputs...),
	}
	return nil
}

func (run *Runner) projExec(output *Chunk, state *OperatorState) (OperatorResult, error) {
	childChunk := &Chunk{}
	var res OperatorResult
	var err error
	if len(run.children) != 0 {
		res, err = run.execChild(run.children[0], childChunk, state)
		if err != nil {
			return 0, err
		}
		if res == InvalidOpResult {
			return InvalidOpResult, nil
		}
	}

	//project list
	projChunk := &Chunk{}
	projChunk.init(run.state.projTypes, defaultVectorSize)
	err = run.state.projExec.executeExprs([]*Chunk{childChunk, nil, nil}, projChunk)
	if err != nil {
		return 0, err
	}

	err = run.state.outputExec.executeExprs([]*Chunk{childChunk, nil, projChunk}, output)
	if err != nil {
		return 0, err
	}

	return res, nil
}
func (run *Runner) projClose() error {

	return nil
}

func (run *Runner) scanInit() error {
	//read schema
	cat, err := tpchCatalog().Table(run.op.Database, run.op.Table)
	if err != nil {
		return err
	}

	run.colIndice = make([]int, 0)
	for _, col := range run.op.Columns {
		if idx, has := cat.Column2Idx[col]; has {
			run.colIndice = append(run.colIndice, idx)
			run.readedColTyps = append(run.readedColTyps, cat.Types[idx].LTyp)
		} else {
			return fmt.Errorf("no such column %s in %s.%s", col, run.op.Database, run.op.Table)
		}
	}

	//open data file
	switch gConf.Format {
	case "parquet":
		run.pqFile, err = pqLocal.NewLocalFileReader(gConf.DataPath + "/" + run.op.Table + ".parquet")
		if err != nil {
			return err
		}

		run.pqReader, err = pqReader.NewParquetColumnReader(run.pqFile, 1)
		if err != nil {
			return err
		}
	case "csv":
		run.tablePath = gConf.DataPath + "/" + run.op.Table + ".tbl"
		run.dataFile, err = os.OpenFile(run.tablePath, os.O_RDONLY, 0755)
		if err != nil {
			return err
		}

		//init csv reader
		run.reader = csv.NewReader(run.dataFile)
		run.reader.Comma = '|'
	default:
		panic("usp format")
	}

	var filterExec *ExprExec
	filterExec, err = initFilterExec(run.op.Filters)
	if err != nil {
		return err
	}

	run.state = &OperatorState{
		filterExec: filterExec,
		filterSel:  NewSelectVector(defaultVectorSize),
		showRaw:    gConf.ShowRaw,
	}

	return nil
}

func (run *Runner) scanExec(output *Chunk, state *OperatorState) (OperatorResult, error) {

	for output.card() == 0 {
		res, err := run.scanRows(output, state, defaultVectorSize)
		if err != nil {
			return InvalidOpResult, err
		}
		if res {
			return Done, nil
		}
	}
	return haveMoreOutput, nil
}

func (run *Runner) scanRows(output *Chunk, state *OperatorState, maxCnt int) (bool, error) {
	if maxCnt == 0 {
		return false, nil
	}
	readed := &Chunk{}
	readed.init(run.readedColTyps, maxCnt)
	var err error
	//read table
	switch gConf.Format {
	case "parquet":
		err = run.readParquetTable(readed, state, maxCnt)
		if err != nil {
			return false, err
		}
	case "csv":
		err = run.readCsvTable(readed, state, maxCnt)
		if err != nil {
			return false, err
		}
	default:
		panic("usp format")
	}

	if readed.card() == 0 {
		return true, nil
	}

	err = run.runFilterExec(readed, output, true)
	if err != nil {
		return false, err
	}
	return false, nil
}

func (run *Runner) scanClose() error {
	switch gConf.Format {
	case "csv":
		run.reader = nil
		return run.dataFile.Close()
	case "parquet":
		run.pqReader.ReadStop()
		return run.pqFile.Close()
	default:
		panic("usp format")
	}
	return nil
}
func (run *Runner) readParquetTable(output *Chunk, state *OperatorState, maxCnt int) error {
	rowCont := -1
	var err error
	var values []interface{}

	//fill field into vector
	for j, idx := range run.colIndice {
		values, _, _, err = run.pqReader.ReadColumnByIndex(int64(idx), int64(maxCnt))
		if err != nil {
			//EOF
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}

		if rowCont < 0 {
			rowCont = len(values)
		} else if len(values) != rowCont {
			return fmt.Errorf("column %d has different count of values %d with previous columns %d", idx, len(values), rowCont)
		}

		vec := output._data[j]
		for i := 0; i < len(values); i++ {
			//[row i, col j]
			val, err := parquetColToValue(values[i], vec.typ())
			if err != nil {
				return err
			}
			vec.setValue(i, val)
			if state.showRaw {
				fmt.Print(values[i], " ")
			}
		}
		if state.showRaw {
			fmt.Println()
		}
	}
	output.setCard(rowCont)

	return nil
}

func (run *Runner) readCsvTable(output *Chunk, state *OperatorState, maxCnt int) error {
	rowCont := 0
	for i := 0; i < maxCnt; i++ {
		//read line
		line, err := run.reader.Read()
		if err != nil {
			//EOF
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
		//fill field into vector
		for j, idx := range run.colIndice {
			if idx >= len(line) {
				return errors.New("no enough fields in the line")
			}
			field := line[idx]
			//[row i, col j] = field
			vec := output._data[j]
			val, err := fieldToValue(field, vec.typ())
			if err != nil {
				return err
			}
			vec.setValue(i, val)
			if state.showRaw {
				fmt.Print(field, " ")
			}
		}
		if state.showRaw {
			fmt.Println()
		}
		rowCont++
	}
	output.setCard(rowCont)

	return nil
}

func fieldToValue(field string, lTyp LType) (*Value, error) {
	var err error
	val := &Value{
		_typ: lTyp,
	}
	switch lTyp.id {
	case LTID_DATE:
		d, err := time.Parse(time.DateOnly, field)
		if err != nil {
			return nil, err
		}
		val._i64 = int64(d.Year())
		val._i64_1 = int64(d.Month())
		val._i64_2 = int64(d.Day())
	case LTID_INTEGER:
		val._i64, err = strconv.ParseInt(field, 10, 64)
		if err != nil {
			return nil, err
		}
	case LTID_VARCHAR:
		val._str = field
	default:
		panic("usp")
	}
	return val, nil
}

func parquetColToValue(field any, lTyp LType) (*Value, error) {
	val := &Value{
		_typ: lTyp,
	}
	switch lTyp.id {
	case LTID_DATE:
		if _, ok := field.(int32); !ok {
			panic("usp")
		}

		d := time.Date(1970, 1, int(1+field.(int32)), 0, 0, 0, 0, time.UTC)
		val._i64 = int64(d.Year())
		val._i64_1 = int64(d.Month())
		val._i64_2 = int64(d.Day())
	case LTID_INTEGER:
		if _, ok := field.(int32); !ok {
			panic("usp")
		}
		val._i64 = int64(field.(int32))
	case LTID_VARCHAR:
		if _, ok := field.(string); !ok {
			panic("usp")
		}

		val._str = field.(string)
	case LTID_DECIMAL:
		switch v := field.(type) {
		case int32:
			val._i64 = int64(v)
		case int64:
			val._i64 = v
		default:
			panic("usp")
		}

	default:
		panic("usp")
	}
	return val, nil
}
