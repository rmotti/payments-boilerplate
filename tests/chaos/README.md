# Chaos tests

The default suite contains deterministic, infrastructure-free fault windows.
The `chaos` build tag adds real TCP interruption and backlog tests. Production
configuration has no fault-injection switch.

Run the short suite on every pull request:

```sh
go test -race -count=1 \
  ./internal/application/outbox \
  ./internal/application/consumer \
  ./internal/adapters/rabbitmq \
  ./tests/chaos
```

Run the long suite only against dedicated, loopback test services:

```sh
CHAOS_DESTRUCTIVE_ALLOWED=1 \
TEST_DATABASE_URL='postgres://payments:payments_local@127.0.0.1:55432/payments?sslmode=disable' \
TEST_RABBITMQ_URL='amqp://payments:payments_local@127.0.0.1:5672/' \
go test -tags=chaos -count=1 -timeout=5m ./tests/chaos
```

The destructive guard is required because the RabbitMQ topology has constant
names. The suite validates loopback targets and purges only
`payments.webhooks` and `payments.webhooks.dlq`. It never truncates PostgreSQL.
Do not run it alongside adapter or E2E tests using those queues.

## Coverage map

| Guarantee | Test |
| --- | --- |
| Publisher nack and mandatory return leave outbox retryable | `TestBrokerNackAndMandatoryReturnKeepMessageEligible`; real mandatory routing remains in `TestUnroutableMessageIsNotReportedAsPublished` |
| Confirm happens before outbox settlement | `TestConfirmedMessageIsRepublishedAfterFailureBeforeSettlement` |
| Two live relays never share a lease; expired leases return | Existing PostgreSQL tests `TestOutboxRelayConcurrentInstancesNeverPublishTwice` and `TestOutboxRelayReclaimsExpiredLease` |
| Failure before consumer commit rolls everything back | Existing PostgreSQL test `TestEventRepositoryRollsBackEverythingOnFailure` |
| Commit happens before ack; redelivery is a no-op | `TestHandleLeavesCommitBeforeAckWindowSafeForRedelivery` and `TestConsumerAcknowledgesOnlyAfterCommittedHandling` |
| Retry budget ends in durable failed state and confirmed DLQ copy | Existing `TestHandleStopsAtTheRetryBudget`, `TestEventRepositoryClosesAnEntryWhenTheRetryBudgetIsSpent`, and RabbitMQ `TestConsumerSendsTerminalFailuresToTheDeadLetterQueue` |
| Failed retry/DLQ republication leaves original unacknowledged | `TestConsumerLeavesOriginalUnacknowledgedWhenRepublishFails` and `TestConsumerLeavesOriginalUnacknowledgedAfterConfirmedCopyIfAckWindowFails` |
| TCP reject, close, blackhole, and restore controls | `TestTCPProxyControlsConnectionFailuresAndRecovery` |
| Real PostgreSQL outage and pool recovery | `TestLongPostgresOutageLeavesConsumerUnacknowledgedAndRecovers` |
| Real RabbitMQ outage, reconnect backoff, relay/consumer recovery, backlog, concurrency, and prefetch | `TestLongRabbitMQRelayRecoversAndDrainsBacklog` and `TestLongRabbitMQConnectionRecoveryAndBacklogMatrix` |

The long backlog test logs OS/architecture, Go version, target, configuration,
volume, elapsed time, and p50/p95 drain latency. Nightly and release-candidate
runs should retain the full `go test -v` output as their report.
