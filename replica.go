package zeitgeber

import (
	"encoding/gob"
	"sync"
	"time"

	"github.com/gitferry/zeitgeber/blockchain"
	"github.com/gitferry/zeitgeber/config"
	"github.com/gitferry/zeitgeber/election"
	"github.com/gitferry/zeitgeber/hotstuff"
	"github.com/gitferry/zeitgeber/identity"
	"github.com/gitferry/zeitgeber/log"
	"github.com/gitferry/zeitgeber/mempool"
	"github.com/gitferry/zeitgeber/message"
	"github.com/gitferry/zeitgeber/pacemaker"
	"github.com/gitferry/zeitgeber/types"
)

type Replica struct {
	Node
	election.Election
	Safety
	pd         *mempool.Producer
	bc         *blockchain.BlockChain
	pm         *pacemaker.Pacemaker
	isStarted  bool
	blockMsg   chan *blockchain.Block
	voteMsg    chan *blockchain.Vote
	qcMsg      chan *blockchain.QC
	timeoutMsg chan *pacemaker.TMO
	newView    chan types.View
	mu         sync.Mutex
}

// NewReplica creates a new replica instance
func NewReplica(id identity.NodeID, alg string, isByz bool) *Replica {
	r := new(Replica)
	r.Node = NewNode(id, isByz)
	if isByz {
		log.Infof("[%v] is Byzantine", r.ID())
	}
	r.Election = election.NewRotation(config.GetConfig().N())
	bc := blockchain.NewBlockchain(config.GetConfig().N())
	r.bc = bc
	r.pd = mempool.NewProducer()
	r.pm = pacemaker.NewPacemaker()
	r.blockMsg = make(chan *blockchain.Block, 1)
	r.voteMsg = make(chan *blockchain.Vote, 1)
	r.qcMsg = make(chan *blockchain.QC, 1)
	r.timeoutMsg = make(chan *pacemaker.TMO, 1)
	r.Register(blockchain.QC{}, r.HandleQC)
	r.Register(blockchain.Block{}, r.HandleBlock)
	r.Register(blockchain.Vote{}, r.HandleVote)
	r.Register(message.Transaction{}, r.handleTxn)
	gob.Register(blockchain.Block{})
	gob.Register(blockchain.QC{})
	gob.Register(blockchain.Vote{})
	switch alg {
	case "hotsutff":
		r.Safety = hotstuff.NewHotStuff(bc, "default")
	default:
		r.Safety = hotstuff.NewHotStuff(bc, "default")
	}
	return r
}

/* Message Handlers */

func (r *Replica) HandleBlock(block blockchain.Block) {
	//log.Debugf("[%v] received a block from %v, view is %v", r.ID(), block.Proposer, block.View)
	if block.View < r.pm.GetCurView() {
		return
	}
	r.blockMsg <- &block
}

func (r *Replica) HandleVote(vote blockchain.Vote) {
	log.Debugf("[%v] received a vote from %v, blockID is %x", r.ID(), vote.Voter, vote.BlockID)
	if vote.View < r.pm.GetCurView() {
		return
	}
	r.voteMsg <- &vote
}

func (r *Replica) HandleQC(qc blockchain.QC) {
	//log.Debugf("[%v] received a qc from, blockID is %x", r.ID(), qc.BlockID)
	if qc.View < r.pm.GetCurView() {
		return
	}
	r.qcMsg <- &qc
}

func (r *Replica) handleTxn(m message.Transaction) {
	//log.Debugf("[%v] received txn %v\n", r.ID(), m)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pd.CollectTxn(&m)
	//	kick-off the protocol
	if !r.isStarted && r.IsLeader(r.ID(), 1) {
		r.isStarted = true
		r.pm.AdvanceView(0)
	}
}

/* Processors */

func (r *Replica) processBlock(block *blockchain.Block) {
	log.Debugf("[%v] is processing block, view: %v, id: %x", r.ID(), block.View, block.ID)
	// TODO: process TC
	r.processCertificate(block.QC)
	curView := r.pm.GetCurView()
	if block.View != curView {
		log.Warningf("[%v] received a stale proposal", r.ID())
		return
	}
	if !r.Election.IsLeader(block.Proposer, block.View) {
		log.Warningf(
			"[%v] received a proposal (%v) from an invalid leader (%v)",
			r.ID(), block.View, block.Proposer)
		return
	}
	r.bc.AddBlock(block)
	shouldVote, err := r.VotingRule(block)
	if err != nil {
		log.Errorf("cannot decide whether to vote the block, %w", err)
		return
	}
	if !shouldVote {
		log.Debugf("[%v] is not going to vote for block, id: %x", r.ID(), block.ID)
		return
	}
	log.Debugf("[%v] is going to vote for block, id: %x", r.ID(), block.ID)
	vote := blockchain.MakeVote(block.View, r.ID(), block.ID)
	err = r.UpdateStateByView(vote.View)
	if err != nil {
		log.Errorf("cannot update state after voting: %w", err)
	}
	// TODO: sign the vote
	time.Sleep(20 * time.Millisecond)
	voteAggregator := r.FindLeaderFor(curView + 1)
	if voteAggregator == r.ID() {
		r.processVote(vote)
	} else {
		r.Send(r.FindLeaderFor(curView+1), vote)
	}
}

func (r *Replica) processCertificate(qc *blockchain.QC) {
	if qc.View < r.pm.GetCurView() {
		return
	}
	r.pm.AdvanceView(qc.View)
	log.Debugf("[%v] has advanced to view %v", r.ID(), r.pm.GetCurView())
	err := r.UpdateStateByQC(qc)
	if err != nil {
		log.Errorf("[%v] cannot update state when processing qc: %w", r.ID(), err)
		return
	}
	// TODO: send the qc to next leader
	//if !r.IsLeader(r.ID(), r.pm.GetCurView()) {
	//	go r.Send(r.FindLeaderFor(r.pm.GetCurView()), qc)
	//}
	if qc.View < 3 {
		return
	}
	ok, block, err := r.CommitRule(qc)
	if err != nil {
		log.Errorf("cannot process the qc %w", err)
		return
	}
	if !ok {
		return
	}
	committedBlocks, err := r.bc.CommitBlock(block.ID)
	if err != nil {
		log.Errorf("[%v] cannot commit blocks", r.ID())
		return
	}
	r.processCommittedBlocks(committedBlocks)
}

func (r *Replica) processCommittedBlocks(blocks []*blockchain.Block) {
	for _, block := range blocks {
		for _, txn := range block.Payload {
			if r.ID() == txn.NodeID {
				txn.Reply(message.TransactionReply{})
			}
			if r.ID() != block.Proposer { // txns are removed when being proposed
				r.pd.RemoveTxn(txn.ID)
			}
		}
		if len(block.Payload) == 0 {
			log.Debugf("[%v] this block has zero payload, id: %x", r.ID(), block.ID)
		}
		log.Debugf("[%v] the block is committed, id: %x", r.ID(), block.ID)
	}
	//	print measurement
	log.Infof("[%v] Committed blocks: %v, total blocks: %v, chain growth: %v", r.ID(), r.bc.GetTotalBlock(), r.bc.GetChainGrowth())
	log.Infof("[%v] Honest committed blocks: %v, committed blocks: %v, chain quality: %v", r.ID(), r.bc.GetHonestCommittedBlock(), r.bc.GetChainQuality())
}

func (r *Replica) processVote(vote *blockchain.Vote) {
	r.mu.Lock()
	isBuilt, qc := r.bc.AddVote(vote)
	r.mu.Unlock()
	if !isBuilt {
		return
	}
	r.processCertificate(qc)
}

func (r *Replica) processNewView(newView types.View) {
	log.Debugf("[%v] is processing new view: %v", r.ID(), newView)
	if !r.IsLeader(r.ID(), newView) {
		return
	}
	r.proposeBlock(newView)
}

func (r *Replica) proposeBlock(view types.View) {
	log.Debugf("[%v] is going to propose block for view: %v", r.ID(), view)
	r.mu.Lock()
	block := r.pd.ProduceBlock(view, r.Safety.Forkchoice(), r.ID())
	r.mu.Unlock()
	//	TODO: sign the block
	// simulate processing time
	time.Sleep(50 * time.Millisecond)
	r.Broadcast(block)
	r.processBlock(block)
	for _, txn := range block.Payload {
		r.pd.RemoveTxn(txn.ID)
	}
}

func (r *Replica) Start() {
	go r.Run()
	for {
		// TODO: add timeout handler
		select {
		case newView := <-r.pm.EnteringViewEvent():
			go r.processNewView(newView)
		case newBlock := <-r.blockMsg:
			go r.processBlock(newBlock)
		case newVote := <-r.voteMsg:
			go r.processVote(newVote)
		case newQC := <-r.qcMsg:
			go r.processCertificate(newQC)
		}
	}
}
