package blockchain

import (
	"fmt"
	"sync"

	"github.com/gitferry/zeitgeber/config"
	"github.com/gitferry/zeitgeber/crypto"
	"github.com/gitferry/zeitgeber/log"
	"github.com/gitferry/zeitgeber/types"
)

type BlockChain struct {
	highQC  *QC
	forrest *LevelledForest
	quorum  *Quorum
	// measurement
	totalBlocks           int
	committedBlocks       int
	honestCommittedBlocks int
	mu                    sync.Mutex
}

func NewBlockchain(n int) *BlockChain {
	bc := new(BlockChain)
	bc.forrest = NewLevelledForest()
	bc.quorum = NewQuorum(n)
	bc.highQC = &QC{View: 0}
	return bc
}

func (bc *BlockChain) AddBlock(block *Block) {
	blockContainer := &BlockContainer{block}
	// TODO: add checks
	bc.forrest.AddVertex(blockContainer)
	err := bc.UpdateHighQC(block.QC)
	if err != nil {
		log.Warningf("found stale qc, view: %v", block.QC.View)
	}
	bc.mu.Lock()
	bc.totalBlocks++
	bc.mu.Unlock()
}

func (bc *BlockChain) AddVote(vote *Vote) (bool, *QC) {
	bc.quorum.Add(vote)
	return bc.GenerateQC(vote.View, vote.BlockID)
}

func (bc *BlockChain) GetHighQC() *QC {
	return bc.highQC
}

func (bc *BlockChain) UpdateHighQC(qc *QC) error {
	if qc.View < bc.highQC.View {
		return fmt.Errorf("cannot update high QC")
	}
	bc.highQC = qc
	return nil
}

func (bc *BlockChain) GenerateQC(view types.View, blockID crypto.Identifier) (bool, *QC) {
	if !bc.quorum.SuperMajority(blockID) {
		return false, nil
	}
	sigs, err := bc.quorum.GetSigs(blockID)
	if err != nil {
		log.Warningf("cannot get signatures, %w", err)
		return false, nil
	}
	qc := &QC{
		View:    view,
		BlockID: blockID,
		AggSig:  sigs,
		// TODO: add real sig
		Signature: nil,
	}

	err = bc.UpdateHighQC(qc)
	if err != nil {
		log.Warningf("generated a stale qc, view: %v", qc.View)
	}

	return true, qc
}

func (bc *BlockChain) CalForkingRate() float32 {
	var forkingRate float32
	//if bc.Height == 0 {
	//	return 0
	//}
	//total := 0
	//for i := 1; i <= bc.Height; i++ {
	//	total += len(bc.Blocks[i])
	//}
	//
	//forkingrate := float32(bc.Height) / float32(total)
	return forkingRate
}

func (bc *BlockChain) GetParentBlock(id crypto.Identifier) (*Block, error) {
	vertex, exists := bc.forrest.GetVertex(id)
	if !exists {
		return nil, fmt.Errorf("the block does not exist, id: %x", id)
	}
	parentID, _ := vertex.Parent()
	parentVertex, exists := bc.forrest.GetVertex(parentID)
	if !exists {
		return nil, fmt.Errorf("parent block does not exist, id: %x", parentID)
	}
	return parentVertex.GetBlock(), nil
}

func (bc *BlockChain) GetGrandParentBlock(id crypto.Identifier) (*Block, error) {
	parentBlock, err := bc.GetParentBlock(id)
	if err != nil {
		return nil, fmt.Errorf("cannot get parent block: %w", err)
	}
	return bc.GetParentBlock(parentBlock.ID)
}

// CommitBlock prunes blocks and returns committed blocks up to the last committed one
func (bc *BlockChain) CommitBlock(id crypto.Identifier) ([]*Block, error) {
	vertex, ok := bc.forrest.GetVertex(id)
	if !ok {
		return nil, fmt.Errorf("cannot find the block, id: %x", id)
	}
	committedNo := vertex.Level() - bc.forrest.LowestLevel
	if committedNo < 1 {
		return nil, fmt.Errorf("cannot commit the block")
	}
	var committedBlocks []*Block
	committedBlocks = append(committedBlocks, vertex.GetBlock())
	for i := uint64(0); i < committedNo-1; i++ {
		parentID, _ := vertex.Parent()
		parentVertex, exists := bc.forrest.GetVertex(parentID)
		if !exists {
			return nil, fmt.Errorf("cannot find the parent block, id: %x", parentID)
		}
		vertex = parentVertex
		if config.Configuration.IsByzantine(vertex.GetBlock().Proposer) {
			bc.mu.Lock()
			bc.honestCommittedBlocks++
			bc.mu.Unlock()
		}
		committedBlocks = append(committedBlocks, vertex.GetBlock())
	}
	err := bc.forrest.PruneUpToLevel(vertex.Level())
	if err != nil {
		return nil, fmt.Errorf("cannot prune the blockchain to the committed block, id: %w", err)
	}
	bc.mu.Lock()
	bc.committedBlocks += len(committedBlocks)
	bc.mu.Unlock()
	return committedBlocks, nil
}

func (bc *BlockChain) GetChildrenBlocks(id crypto.Identifier) []*Block {
	var blocks []*Block
	iterator := bc.forrest.GetChildren(id)
	for I := iterator; I.HasNext(); {
		blocks = append(blocks, I.NextVertex().GetBlock())
	}
	return blocks
}

func (bc *BlockChain) GetChainGrowth() float64 {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	return float64(bc.committedBlocks) / float64(bc.totalBlocks)
}

func (bc *BlockChain) GetChainQuality() float64 {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	return float64(bc.honestCommittedBlocks) / float64(bc.committedBlocks)
}

func (bc *BlockChain) GetTotalBlock() int {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	return bc.totalBlocks
}

func (bc *BlockChain) GetHonestCommittedBlock() int {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	return bc.honestCommittedBlocks
}

func (bc *BlockChain) GetCommittedBlock() int {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	return bc.committedBlocks
}
