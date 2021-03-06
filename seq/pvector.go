// Copyright 2014 David Miller. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package seq

import (
	"errors"
	"github.com/dmiller/go-seq/iseq"
	"github.com/dmiller/go-seq/sequtil"
)

// PVector implements a persistent vector via a specialized form of array-mapped hash trie.
type PVector struct {
	cnt   int
	shift uint
	root  *vnode
	tail  []interface{}
	AMeta
	hash uint32
}

// vnode is a node in the trie for PVector
type vnode struct {
	array []interface{}
}

// these constants are related to the 32-way branching in nodes
const (
	baseShift    = 5 // log-base-2 of 32
	branchFactor = 1 << baseShift
	indexMask    = 0x01f // rightmost baseShift bits masked
)

var (
	emptyVnode = &vnode{array: make([]interface{}, branchFactor)}

	// EmptyPVector represents a PVector with zero elements.
	// The zero-value for PVector is not valid.
	// Use this value if you need an empty PVector.
	EmptyPVector = &PVector{cnt: 0, shift: baseShift, root: emptyVnode, tail: make([]interface{}, 0)}
)

// ctors

// Create a PVector from an ISeq
func NewPVectorFromISeq(items iseq.Seq) *PVector {
	// TODO: redo when we have transients
	var ret iseq.PVector = EmptyPVector
	for ; items != nil; items = items.Next() {
		ret = ret.ConsV(items.First())
	}
	return ret.(*PVector)
}

// Create a PVector from a slice (of interface{})
func NewPVectorFromSlice(items []interface{}) *PVector {
	// TODO: redo when we have transients
	var ret iseq.PVector = EmptyPVector
	for _, item := range items {
		ret = ret.ConsV(item)
	}
	return ret.(*PVector)
}

// Create a PVector from the given arguments
func NewPVectorFromItems(items ...interface{}) *PVector {
	return NewPVectorFromSlice(items)
}

//  PVector needs to implement the following iseq interfaces:
//        Meta MetaW Seqable PCollection Lookup Associative PStack PVector Counted Reversible Indexed
//  Are we going to do EditableCollection?
//  Also, Equivable and Hashable
//
// interface Meta is covered by the AMeta embedding

// interface MetaW

func (v *PVector) WithMeta(meta iseq.PMap) iseq.MetaW {
	return &PVector{AMeta: AMeta{meta}, cnt: v.cnt, shift: v.shift, root: v.root, tail: v.tail}
}

// interface Seqable members

func (v *PVector) Seq() iseq.Seq {
	if cs := v.chunkedSeq(); cs != nil {
		// avoid the dreaded nil interface problem
		return cs
	}
	return nil
}

func (v *PVector) chunkedSeq() *chunkedSeq {
	if v.cnt == 0 {
		return nil
	}
	return newChunkedSeq(v, 0, 0)
}

// interface PCollection

func (v *PVector) Count() int {
	return v.cnt
}

func (v *PVector) Cons(o interface{}) iseq.PCollection {
	return v.ConsV(o)
}

func (v *PVector) Empty() iseq.PCollection {
	return CachedEmptyList.WithMeta(v.Meta()).(iseq.PCollection)
}

// interface Counted

func (v *PVector) Count1() int {
	return v.cnt
}

// interface Indexed

func (v *PVector) Nth(i int) interface{} {
	node := v.arrayFor(i)
	return node[i&indexMask]
}

func (v *PVector) NthD(i int, notFound interface{}) interface{} {
	if i >= 0 && i < v.cnt {
		return v.Nth(i)
	}
	return notFound
}

func (v *PVector) NthE(i int) (interface{}, error) {
	if i >= 0 && i < v.cnt {
		return v.Nth(i), nil
	}
	return nil, errors.New("Index out of bounds in PVector")
}

// interface Lookup

func (v *PVector) ValAt(key interface{}) interface{} {
	return v.ValAtD(key, nil)
}

func (v *PVector) ValAtD(key interface{}, notFound interface{}) interface{} {
	if i, ok := key.(int); ok && i >= 0 && i < v.cnt {
		return v.Nth(i)
	}
	return notFound
}

// interface Associative

func (v *PVector) ContainsKey(key interface{}) bool {
	i, ok := key.(int)
	return ok && i >= 0 && i < v.cnt
}

func (v *PVector) EntryAt(key interface{}) iseq.MapEntry {
	if i, ok := key.(int); ok && i >= 0 && i < v.cnt {
		return MapEntry{key, v.Nth(i)}
	}
	return nil
}

func (v *PVector) Assoc(key interface{}, val interface{}) iseq.Associative {
	if i, ok := key.(int); ok {
		return v.AssocN(i, val)
	}
	panic("Index must be an integer")
}

// interface PVector

// ConsV creates a new vector with a new item at the end
func (v *PVector) ConsV(o interface{}) iseq.PVector {
	if v.cnt-v.tailoff() < branchFactor {
		newTail := make([]interface{}, len(v.tail)+1)
		copy(newTail, v.tail)
		newTail[len(v.tail)] = o
		return &PVector{AMeta: AMeta{v.meta}, cnt: v.cnt + 1, shift: v.shift, root: v.root, tail: newTail}
	}
	// full tail, push into tree
	tailNode := &vnode{v.tail}
	newShift := v.shift

	var newRoot *vnode

	// overflow root?
	if (v.cnt >> baseShift) > (1 << v.shift) {
		newRoot = &vnode{make([]interface{}, branchFactor)}
		newRoot.array[0] = v.root
		newRoot.array[1] = newPath(v.shift, tailNode)
		newShift = newShift + baseShift
	} else {
		newRoot = v.pushTail(v.shift, v.root, tailNode)
	}

	return &PVector{AMeta: AMeta{v.meta}, cnt: v.cnt + 1, shift: newShift, root: newRoot, tail: []interface{}{o}}
}

func (v *PVector) pushTail(level uint, parent *vnode, tailNode *vnode) *vnode {
	// if parent is leaf, insert node,
	// else does it map to existing child?  -> nodeToInsert = pushNode one more level
	// else alloc new path
	// return nodeToInsert placed in copy of parent
	subidx := ((v.cnt - 1) >> level) & indexMask
	newArray := make([]interface{}, len(parent.array))
	copy(newArray, parent.array)
	ret := &vnode{newArray}

	var nodeToInsert *vnode
	if level == baseShift {
		nodeToInsert = tailNode
	} else {
		if child, ok := parent.array[subidx].(*vnode); ok {
			nodeToInsert = v.pushTail(level-baseShift, child, tailNode)
		} else {
			nodeToInsert = newPath(level-baseShift, tailNode)
		}
	}
	ret.array[subidx] = nodeToInsert
	return ret
}

func newPath(level uint, node *vnode) *vnode {
	if level == 0 {
		return node
	}

	ret := vnode{array: make([]interface{}, branchFactor)}
	ret.array[0] = newPath(level-baseShift, node)
	return &ret
}

// AssocV returns a new vector with the i-th value set to the given value
func (v *PVector) AssocN(i int, val interface{}) iseq.PVector {
	if i >= 0 && i < v.cnt {
		if i >= v.tailoff() {
			newTail := make([]interface{}, len(v.tail))
			copy(newTail, v.tail)
			newTail[i&indexMask] = val
			return &PVector{AMeta: AMeta{v.meta}, cnt: v.cnt, shift: v.shift, root: v.root, tail: newTail}
		}
		return &PVector{AMeta: AMeta{v.meta}, cnt: v.cnt, shift: v.shift, root: doAssoc(v.shift, v.root, i, val), tail: v.tail}

	} else if i == v.cnt {
		return v.ConsV(val)
	}

	panic("Argument out of range")

}

func doAssoc(level uint, node *vnode, i int, val interface{}) *vnode {
	newArray := make([]interface{}, len(node.array))
	copy(newArray, node.array)
	if level == 0 {
		newArray[i&indexMask] = val
	} else {
		subidx := (i >> level) & indexMask
		newArray[subidx] = doAssoc(level-baseShift, (node.array[subidx]).(*vnode), i, val)
	}
	return &vnode{array: newArray}
}

// interface PStack

func (v *PVector) Peek() interface{} {
	if v.cnt > 0 {
		return v.Nth(v.cnt - 1)
	}
	return nil
}

func (v *PVector) Pop() iseq.PStack {
	// TODO: convert to switch
	if v.cnt == 0 {
		// TODO: determine if pop should have other behavior
		panic("Can't pop empty vector")
	}

	if v.cnt == 1 {
		return EmptyPVector.WithMeta(v.meta).(iseq.PStack)
	}

	if v.cnt-v.tailoff() > 1 {
		newTail := make([]interface{}, len(v.tail)-1)
		copy(newTail, v.tail)
		return &PVector{AMeta: AMeta{v.meta}, cnt: v.cnt - 1, shift: v.shift, root: v.root, tail: newTail}
	}

	newTail := v.arrayFor(v.cnt - 2)
	newRoot := v.popTail(v.shift, v.root)
	newShift := v.shift

	if newRoot == nil {
		newRoot = emptyVnode
	}
	if v.shift > 5 && newRoot.array[1] == nil {
		newRoot, _ = newRoot.array[0].(*vnode)
		// x := newRoot.array[0]
		// if x == nil {
		// 	newRoot = nil
		// } else {
		// 	newRoot = x.(*vnode)
		// }
		newShift = newShift - baseShift
	}
	return &PVector{AMeta: AMeta{v.meta}, cnt: v.cnt - 1, shift: newShift, root: newRoot, tail: newTail}
}

func (v *PVector) popTail(level uint, node *vnode) *vnode {
	subidx := ((v.cnt - 2) >> level) & indexMask
	if level > baseShift {
		newChild := v.popTail(level-baseShift, node.array[subidx].(*vnode))
		if newChild == nil && subidx == 0 {
			return nil
		}
		newArray := make([]interface{}, len(node.array))
		copy(newArray, node.array)
		return &vnode{newArray}
	} else if subidx == 0 {
		return nil
	}

	newArray := make([]interface{}, len(node.array))
	copy(newArray, node.array)
	newArray[subidx] = nil
	return &vnode{newArray}
}

// interface Reversible

func (v *PVector) Rseq() iseq.Seq {
	// TODO: implment Rseq
	return nil
}

// utilities

func (v *PVector) arrayFor(i int) []interface{} {
	if i < 0 && i >= v.cnt {
		// TODO: create error objects for all panics
		// THis is a panic in the same way as any array index out-of-bounds
		panic("Array index out of bounds")
	}

	if i >= v.tailoff() {
		return v.tail
	}

	node := v.root
	for level := v.shift; level > 0; level -= baseShift {
		node = node.array[(i>>level)&indexMask].(*vnode)
	}
	return node.array
}

func (v *PVector) tailoff() int {
	if v.cnt < branchFactor {
		return 0
	}
	return ((v.cnt - 1) >> baseShift) << baseShift
}

// interfaces Equivable, Hashable

func (p *PVector) Equiv(o interface{}) bool {

	if p == o {
		return true
	}

	if ov, ok := o.(iseq.PVector); ok {
		if p.Count1() != ov.Count1() {
			return false
		}

		for i := 0; i < p.Count1(); i++ {
			if !sequtil.Equiv(p.Nth(i), ov.Nth(i)) {
				return false
			}
		}
		return true
	}

	// TODO: when we have Sequential, fix this
	if os, ok := o.(iseq.Seqable); ok {
		s := os.Seq()
		for i := 0; i < p.Count1(); i, s = i+1, s.Next() {
			if s == nil || !sequtil.Equiv(p.Nth(i), s.First()) {
				return false
			}
		}
		if s != nil {
			return false
		}

		return true
	}

	// TODO: handle built-in 'sequable' things such as arrays, slices, strings
	return false
}

func (p *PVector) Hash() uint32 {
	if p.hash == 0 {
		p.hash = sequtil.HashSeq(p.Seq())
	}

	return p.hash
}

/*
   static readonly AtomicReference<Thread> NoEdit = new AtomicReference<Thread>(null);



   static public PVector create1(ICollection items)
   {
       ITransientCollection ret = EMPTY.asTransient();
       foreach (object item in items)
           ret = ret.conj(item);
       return (PVector)ret.P();
   }

   public PVector(int cnt, int shift, Node root, object[] tail)
   {
       _meta = null;
       _cnt = cnt;
       _shift = shift;
       _root = root;
       _tail = tail;
   }

   PVector(IPMap meta, int cnt, int shift, Node root, object[] tail)
   {
       _meta = meta;
       _cnt = cnt;
       _shift = shift;
       _root = root;
       _tail = tail;
   }



*/
