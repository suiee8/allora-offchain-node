package usecase

import (
	"allora_offchain_node/lib"
	"sync"

	emissions "github.com/allora-network/allora-chain/x/emissions/types"
	"github.com/rs/zerolog/log"
)

func (suite *UseCaseSuite) Spawn() {
	var wg sync.WaitGroup

	// Run worker process per topic
	alreadyStartedWorkerForTopic := make(map[emissions.TopicId]bool)
	for _, worker := range suite.Node.Worker {
		if _, ok := alreadyStartedWorkerForTopic[worker.TopicId]; ok {
			log.Debug().Uint64("topicId", worker.TopicId).Msg("Worker already started for topicId")
			continue
		}
		alreadyStartedWorkerForTopic[worker.TopicId] = true

		wg.Add(1)
		go func(worker lib.WorkerConfig) {
			defer wg.Done()
			suite.runWorkerProcess(worker)
		}(worker)
	}

	// Run reputer process per topic
	alreadyStartedReputerForTopic := make(map[emissions.TopicId]bool)
	for _, reputer := range suite.Node.Reputer {
		if _, ok := alreadyStartedReputerForTopic[reputer.TopicId]; ok {
			log.Debug().Uint64("topicId", reputer.TopicId).Msg("Reputer already started for topicId")
			continue
		}
		alreadyStartedReputerForTopic[reputer.TopicId] = true

		wg.Add(1)
		go func(reputer lib.ReputerConfig) {
			defer wg.Done()
			suite.runReputerProcess(reputer)
		}(reputer)
	}

	// Wait for all goroutines to finish
	wg.Wait()
}

func (suite *UseCaseSuite) runWorkerProcess(worker lib.WorkerConfig) {
	log.Info().Uint64("topicId", worker.TopicId).Msg("Running worker process for topic")
	println("Running worker process for topic", worker.TopicId)

	topic, err := suite.Node.GetTopicById(worker.TopicId)
	if err != nil {
		log.Error().Err(err).Uint64("topicId", worker.TopicId).Msg("Failed to get topic")
		return
	}

	registered := suite.Node.RegisterWorkerIdempotently(worker)
	if !registered {
		log.Error().Err(err).Uint64("topicId", worker.TopicId).Msg("Failed to register worker for topic")
		return
	}

	mustRecalcWindow := true
	window := AnticipatedWindow{}
	for {
		currentBlock, err := suite.Node.GetCurrentChainBlockHeight()
		if err != nil {
			log.Error().Err(err).Uint64("topicId", worker.TopicId).Msg("Error getting chain block height for worker job on topic")
			return
		}

		if mustRecalcWindow {

			window = window.CalcWorkerSoonestAnticipatedWindow(suite, topic, currentBlock)
			log.Debug().Msgf("Worker anticipated window for topic open nonce %d: %v", worker.TopicId, window)
			mustRecalcWindow = false
		}

		if window.BlockIsWithinWindow(currentBlock) {
			attemptCommit := true

			latestOpenWorkerNonce, err := suite.Node.GetLatestOpenWorkerNonceByTopicId(worker.TopicId)
			if latestOpenWorkerNonce.BlockHeight == 0 || err != nil {
				log.Error().Err(err).Uint64("topicId", worker.TopicId).Msg("Error getting latest open worker nonce on topic")
				attemptCommit = false // Wait some time and try again if block still within the anticipated window
			}

			if attemptCommit {
				success, err := suite.BuildCommitWorkerPayload(worker, latestOpenWorkerNonce)
				if err != nil {
					log.Error().Err(err).Uint64("topicId", worker.TopicId).Msg("Error building and committing worker payload for topic")
				}
				if success {
					mustRecalcWindow = true
					window.WaitForNextAnticipatedWindowToStart(currentBlock, topic.EpochLength)
					continue
				}
			}

			suite.WaitWithinAnticipatedWindow()
		} else {
			window.WaitForNextAnticipatedWindowToStart(currentBlock, topic.EpochLength)
		}
	}
}

func (suite *UseCaseSuite) runReputerProcess(reputer lib.ReputerConfig) {
	log.Debug().Uint64("topicId", reputer.TopicId).Msg("Running reputer process for topic")

	topic, err := suite.Node.GetTopicById(reputer.TopicId)
	if err != nil {
		log.Error().Err(err).Uint64("topicId", reputer.TopicId).Msg("Failed to get topic")
		return
	}

	registeredAndStaked := suite.Node.RegisterAndStakeReputerIdempotently(reputer)
	if !registeredAndStaked {
		log.Error().Err(err).Uint64("topicId", reputer.TopicId).Msg("Failed to register or sufficiently stake reputer for topic")
		return
	}

	var latestOpenReputerNonce lib.BlockHeight
	mustRecalcOpenNonceWindow := true
	mustGetOpenNonce := true
	mustRecalcReputerWindow := true
	window := AnticipatedWindow{}
	for {
		currentBlock, err := suite.Node.GetCurrentChainBlockHeight()
		if err != nil {
			log.Error().Err(err).Uint64("topicId", reputer.TopicId).Msg("Error getting chain block height for reputer job on topic")
			return
		}

		// Try to get the open nonce for the reputer
		if mustRecalcOpenNonceWindow {
			window = window.CalcWorkerSoonestAnticipatedWindow(suite, topic, currentBlock)
			log.Debug().Msgf("Reputer anticipated window for open nonce for topic %d: %v", reputer.TopicId, window)
			mustRecalcOpenNonceWindow = false
		}

		if mustGetOpenNonce && window.BlockIsWithinWindow(currentBlock) {
			latestOpenReputerNonce, err = suite.Node.GetLatestOpenReputerNonceByTopicId(reputer.TopicId)
			if latestOpenReputerNonce == 0 || err != nil {
				log.Error().Err(err).Uint64("topicId", reputer.TopicId).Msg("Error getting latest open reputer nonce on topic")
				mustGetOpenNonce = true
				continue
			}
			mustGetOpenNonce = false
		}

		if mustRecalcReputerWindow {
			window = *window.CalcReputerSoonestAnticipatedWindow(topic, latestOpenReputerNonce)
			log.Debug().Msgf("Reputer anticipated window for submission for topic %d: %v", reputer.TopicId, window)
			mustRecalcReputerWindow = false
		}

		if window.BlockIsWithinReputerWindow(currentBlock) {
			success, err := suite.BuildCommitReputerPayload(reputer, latestOpenReputerNonce)
			if err != nil {
				log.Error().Err(err).Uint64("topicId", reputer.TopicId).Msg("Error building and committing worker payload for topic")
			}
			if success {
				mustRecalcOpenNonceWindow = true
				mustGetOpenNonce = true
				mustRecalcReputerWindow = true
				window.WaitForNextAnticipatedWindowToStart(currentBlock, topic.EpochLength)
				continue
			} else {
				suite.WaitWithinAnticipatedWindow()
			}

		} else {
			window.WaitForNextReputerAnticipatedWindowToStart(topic, latestOpenReputerNonce, currentBlock)
		}
	}
}