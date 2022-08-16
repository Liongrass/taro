package mssmt

const (
	// MaxTreeLevels represents the depth of the MS-SMT.
	MaxTreeLevels = hashSize * 8

	// lastBitIndex represents the index of the last bit for MS-SMT keys.
	lastBitIndex = MaxTreeLevels - 1
)

var (
	// EmptyTree stores a copy of all nodes up to the root in a MS-SMT in
	// which all the leaves are empty.
	EmptyTree []Node
)

func init() {
	// Force the calculation of the node key for the empty node. This will
	// ensure the value is fully cached for the loop below.
	EmptyLeafNode.NodeKey()

	// Initialize the empty MS-SMT by starting from an empty leaf and
	// hashing all the way up to the root.
	EmptyTree = make([]Node, MaxTreeLevels+1)
	EmptyTree[MaxTreeLevels] = EmptyLeafNode
	for i := lastBitIndex; i >= 0; i-- {
		// Create the branch and force the calculation of the node key.
		// At this point we already have computed the keys of each of
		// the siblings, so those cached values can be used here. If we
		// don't do this, then concurrent callers will attempt to
		// read/populate this value causing a race condition.
		branch := NewBranch(EmptyTree[i+1], EmptyTree[i+1])
		branch.NodeKey()

		EmptyTree[i] = branch
	}
}

// FullTree represents a Merkle-Sum Sparse Merkle Tree (MS-SMT). A MS-SMT is an
// augmented version of a sparse merkle tree that includes a sum value, which is
// combined during the internal branch hashing operation. Such trees permit
// efficient proofs of non-inclusion, while also supporting efficient fault
// proofs of invalid merkle sum commitments.
type FullTree struct {
	root  Node
	store Store
}

var _ Tree = (*FullTree)(nil)

// NewFullTree initializes an empty MS-SMT backed by `store`. As a result,
// `store` will only maintain non-empty relevant nodes, i.e., stale parents are
// deleted and empty nodes are never stored.
func NewFullTree(store Store) *FullTree {
	return &FullTree{
		root:  EmptyTree[0],
		store: store,
	}
}

// Root returns the root node of the MS-SMT.
func (t *FullTree) Root() *BranchNode {
	return t.root.(*BranchNode)
}

// bitIndex returns the bit found at `idx` for a NodeKey.
func bitIndex(idx uint8, key *[hashSize]byte) byte {
	byteVal := key[idx/8]
	return (byteVal >> (idx % 8)) & 1
}

// Type alias for closures to be invoked at every iteration of walking through a
// tree.
type iterFunc = func(height uint8, current, sibling, parent Node)

// walkDown walks down the tree from the root node to the leaf indexed by `key`.
// The leaf node found is returned.
func (t *FullTree) walkDown(key *[hashSize]byte, iter iterFunc) *LeafNode {
	current := t.root
	for i := 0; i <= lastBitIndex; i++ {
		left, right := t.store.GetChildren(uint8(i), current.NodeKey())
		var next, sibling Node
		if bitIndex(uint8(i), key) == 0 {
			next, sibling = left, right
		} else {
			next, sibling = right, left
		}
		if iter != nil {
			iter(uint8(i), next, sibling, current)
		}
		current = next
	}
	return current.(*LeafNode)
}

// walkUp walks up from the `start` leaf node up to the root with the help of
// `siblings`. The root branch node computed is returned.
func walkUp(key *[hashSize]byte, start *LeafNode, siblings []Node,
	iter iterFunc) *BranchNode {

	var current Node = start
	for i := lastBitIndex; i >= 0; i-- {
		sibling := siblings[lastBitIndex-i]
		var parent Node
		if bitIndex(uint8(i), key) == 0 {
			parent = NewBranch(current, sibling)
		} else {
			parent = NewBranch(sibling, current)
		}
		if iter != nil {
			iter(uint8(i), current, sibling, parent)
		}
		current = parent
	}
	return current.(*BranchNode)
}

// insert inserts a leaf node at the given key within the MS-SMT.
func (t *FullTree) insert(key *[hashSize]byte, leaf *LeafNode) *FullTree {
	// As we walk down to the leaf node, we'll keep track of the sibling and
	// parent for each node we visit.
	prevParents := make([]NodeKey, MaxTreeLevels)
	siblings := make([]Node, MaxTreeLevels)
	_ = t.walkDown(key, func(i uint8, _, sibling, parent Node) {
		prevParents[MaxTreeLevels-1-i] = parent.NodeKey()
		siblings[MaxTreeLevels-1-i] = sibling
	})

	// Now that we've arrived at the leaf node, we'll need to work our way
	// back up to the root, updating any stale and new intermediate branch
	// nodes.
	root := walkUp(key, leaf, siblings, func(i uint8, _, _, parent Node) {
		// Replace the old parent with the new one. Our store should
		// never track empty branches.
		prevParent := prevParents[MaxTreeLevels-1-i]
		if prevParent != EmptyTree[i].NodeKey() {
			t.store.DeleteBranch(prevParent)
		}
		if parent.NodeKey() != EmptyTree[i].NodeKey() {
			t.store.InsertBranch(parent.(*BranchNode))
		}
	})

	// With our new root updated, we can update the leaf node within the
	// store. If we've inserted an empty leaf, then the leaf node found at
	// the given key is being deleted, otherise it's being inserted.
	if leaf.IsEmpty() {
		t.store.DeleteLeaf(*key)
	} else {
		t.store.InsertLeaf(leaf)
	}
	t.root = root
	return t
}

// Insert inserts a leaf node at the given key within the MS-SMT.
func (t *FullTree) Insert(key [hashSize]byte, leaf *LeafNode) Tree {
	return t.insert(&key, leaf)
}

// Delete deletes the leaf node found at the given key within the MS-SMT.
func (t *FullTree) Delete(key [hashSize]byte) Tree {
	return t.insert(&key, EmptyLeafNode)
}

// Get returns the leaf node found at the given key within the MS-SMT.
func (t *FullTree) Get(key [hashSize]byte) *LeafNode {
	return t.walkDown(&key, nil)
}

// MerkleProof generates a merkle proof for the leaf node found at the given key
// within the MS-SMT. If a leaf node does not exist at the given key, then the
// proof should be considered a non-inclusion proof. This is noted by the
// returned `Proof` containing an empty leaf.
func (t *FullTree) MerkleProof(key [hashSize]byte) *Proof {
	proof := make([]Node, MaxTreeLevels)
	_ = t.walkDown(&key, func(i uint8, _, sibling, _ Node) {
		proof[MaxTreeLevels-1-i] = sibling
	})
	return NewProof(proof)
}

// VerifyMerkleProof determines whether a merkle proof for the leaf found at the
// given key is valid.
func VerifyMerkleProof(key [hashSize]byte, leaf *LeafNode, proof *Proof,
	root Node) bool {

	return IsEqualNode(proof.Root(key, leaf), root)
}
