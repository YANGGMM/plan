package main

import (
	"bytes"
	"fmt"
	"github.com/stretchr/testify/assert"
	"testing"
)

func Test_Graph(t *testing.T) {
	g := NewGraph()

	g.AddEdge(&GNode{index: 1, db: "db1", name: "t1"}, &GNode{index: 2, db: "db1", name: "t2"})
	g.AddEdge(&GNode{index: 1, db: "db1", name: "t1"}, &GNode{index: 3, db: "db1", name: "t3"})
	g.AddEdge(&GNode{index: 2, db: "db1", name: "t2"}, &GNode{index: 3, db: "db1", name: "t3"})
	fmt.Println(g.String())
}

func TestTreeNode(t *testing.T) {
	root := newTreeNode()
	root.relation = NewJoinRelationSet([]uint64{1, 2, 3})
	root.children[1] = newTreeNode()
	root.children[2] = newTreeNode()
	root.children[3] = newTreeNode()
	fmt.Println(root.String())
}

func TestNewJoinRelationSetManager(t *testing.T) {
	m := NewJoinRelationSetManager()
	set := make(Set)
	set.insert(1, 2, 3, 4, 5, 6, 7)
	m.getRelation(set)
	set = make(Set)
	set.insert(1, 2, 3, 4)
	m.getRelation(set)
	fmt.Println(m)
}

func TestGraph(t *testing.T) {
	m := NewJoinRelationSetManager()
	set := make(Set)
	set.insert(1, 2, 3, 4, 5, 6, 7)
	jset1 := m.getRelation(set)
	set = make(Set)
	set.insert(1, 2, 3, 4)
	jset2 := m.getRelation(set)
	set = make(Set)
	set.insert(3, 4, 5)
	jset3 := m.getRelation(set)
	set = make(Set)
	set.insert(9, 8)
	jset4 := m.getRelation(set)
	fmt.Println(m)

	g := NewQueryGraph()
	g.CreateEdge(jset1, jset2, nil)
	g.CreateEdge(jset1, jset3, nil)
	g.CreateEdge(jset4, jset1, nil)
	fmt.Println(g)

	checkConn := func(a, b *JoinRelationSet, has bool) {
		conns := g.GetConnections(a, b)
		if has {
			if len(conns) == 0 {
				t.Fatal("must have connections between ", a, b)
			}
			for _, conn := range conns {
				fmt.Println(conn.neighbor)
			}
		} else {
			if len(conns) != 0 {
				t.Fatal("must not have connections between ", a, b)
			}
		}

	}
	checkConn(jset4, jset1, true)
	checkConn(jset1, jset4, false)
	checkConn(jset1, jset2, true)
	checkConn(jset2, jset1, false)
	checkConn(jset1, jset3, true)
	checkConn(jset3, jset1, false)
	checkConn(jset2, jset3, false)
	checkConn(jset3, jset2, false)
	checkConn(jset3, jset4, false)
	checkConn(jset4, jset3, false)
	checkConn(jset4, jset2, false)
	checkConn(jset2, jset4, false)

}

type Compare interface {
	Equal(Compare) bool
	Name() []byte
}

type ABC struct {
	name []byte
}

func (a *ABC) Equal(o Compare) bool {
	return bytes.Compare(a.name, o.(*ABC).name) == 0
}

func (a *ABC) Name() []byte {
	return a.name
}

func TestXXX(t *testing.T) {
	x := make(map[Compare]int)
	x[&ABC{name: []byte("abc")}] = 1
	x[&ABC{name: []byte("def")}] = 1
	for key := range x {
		fmt.Println(key.Name())
	}
}

func TestPlanMap(t *testing.T) {
	pm := make(planMap)
	s1 := NewJoinRelationSet([]uint64{1, 2, 3})
	s2 := NewJoinRelationSet([]uint64{1, 2, 3})
	pm.set(s1, &JoinNode{})
	assert.NotNil(t, pm.get(s1))
	assert.NotNil(t, pm.get(s2))

}
