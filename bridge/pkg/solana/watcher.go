package ethereum

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.uber.org/zap"

	agentv1 "github.com/certusone/wormhole/bridge/pkg/proto/agent/v1"
	"github.com/certusone/wormhole/bridge/pkg/readiness"

	"github.com/certusone/wormhole/bridge/pkg/common"
	"github.com/certusone/wormhole/bridge/pkg/supervisor"
	"github.com/certusone/wormhole/bridge/pkg/vaa"
)

type (
	SolanaVAASubmitter struct {
		url string

		lockChan chan *common.ChainLock
		vaaChan  chan *vaa.VAA
	}
)

func NewSolanaVAASubmitter(url string, lockEvents chan *common.ChainLock, vaaQueue chan *vaa.VAA) *SolanaVAASubmitter {
	return &SolanaVAASubmitter{url: url, lockChan: lockEvents, vaaChan: vaaQueue}
}

func (e *SolanaVAASubmitter) Run(ctx context.Context) error {
	// We only support UNIX sockets since we rely on Unix filesystem permissions for access control.
	path := fmt.Sprintf("unix://%s", e.url)

	timeout, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(timeout, path, grpc.WithBlock(), grpc.WithInsecure())
	if err != nil {
		return fmt.Errorf("failed to dial agent at %s: %w", path, err)
	}
	defer conn.Close()

	c := agentv1.NewAgentClient(conn)

	errC := make(chan error)
	logger := supervisor.Logger(ctx)

	// Check whether agent is up by doing a GetBalance call. This is a bit hacky, but otherwise, a broken agent won't
	// fail until Recv(). Readiness is best-effort and if this succeeds, it's fair to assume that the watch does too.
	balance, err := c.GetBalance(timeout, &agentv1.GetBalanceRequest{})
	if err != nil {
		return fmt.Errorf("failed to get balance: %v", err)
	}
	readiness.SetReady(common.ReadinessSolanaSyncing)
	logger.Info("account balance", zap.Uint64("lamports", balance.Balance))

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case v := <-e.vaaChan:
				vaaBytes, err := v.Marshal()
				if err != nil {
					panic(err)
				}

				// Calculate digest so we can log it (TODO: refactor to vaa method? we do this in different places)
				m, err := v.SigningMsg()
				if err != nil {
					panic(err)
				}
				h := hex.EncodeToString(m.Bytes())

				timeout, cancel := context.WithTimeout(ctx, 15*time.Second)
				res, err := c.SubmitVAA(timeout, &agentv1.SubmitVAARequest{Vaa: vaaBytes})
				cancel()
				if err != nil {
					st, ok := status.FromError(err)
					if !ok {
						panic("err not a status")
					}

					// For transient errors, we can put the VAA back into the queue such that it can
					// be retried after the runnable has been rescheduled.
					switch st.Code() {
					case
						// Our context was cancelled, likely because the watcher stream died.
						codes.Canceled,
						// The agent encountered a transient error, likely node unavailability.
						codes.Unavailable,
						codes.Aborted:

						logger.Error("transient error, requeuing VAA", zap.Error(err), zap.String("digest", h))

						// Tombstone goroutine
						go func(v *vaa.VAA) {
							time.Sleep(10 * time.Second)
							e.vaaChan <- v
						}(v)

					case codes.Internal:
						// This VAA has already been executed on chain, successfully or not.
						// TODO: dissect InstructionError in agent and convert this to the proper gRPC code
						if strings.Contains(st.Message(), "custom program error: 0xb") { // AlreadyExists
							logger.Info("VAA already submitted on-chain, ignoring", zap.Error(err), zap.String("digest", h))
							break
						}

						fallthrough
					default:
						logger.Error("error submitting VAA", zap.Error(err), zap.String("digest", h))
					}

					break
				}

				logger.Info("submitted VAA",
					zap.String("tx_sig", res.Signature), zap.String("digest", h))
			}
		}
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errC:
		return err
	}
}
