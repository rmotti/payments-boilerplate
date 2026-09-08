# ADR 0015: métricas da aplicação, cardinalidade e coleta de estado

- Status: aceito
- Data: 2026-09-07
- Implementação: E5a e E5b da Fase 4

## Contexto

O projeto já exportava sinais por OTLP antes desta decisão, mas nenhum deles
falava o vocabulário da aplicação. Havia duração de requisição HTTP pelo
`otelhttp` e instrumentação de runtime do Go. Nada dizia quantos eventos foram
recebidos, quantas assinaturas falharam, quanto tempo o relay leva para
publicar, quantas mensagens estão na DLQ ou há quanto tempo a mensagem mais
antiga espera no outbox.

O `docs/metrics.md` listava vinte e um nomes de instrumentos sem distinguir o
que existia do que era intenção. Um operador que montasse um painel a partir
daquela lista teria painéis permanentemente vazios e não saberia por quê.

Existem três restrições que moldam a decisão.

A primeira é de acoplamento. O domínio e os casos de uso não podem importar
OpenTelemetry. As camadas de aplicação e de adapter já expõem interfaces de
observer — `outbox.Observer`, `consumer.Observer`, `rabbitmq.ConsumerObserver` —
que o worker implementa com Zap. Essas interfaces são a fronteira certa, e
substituí-las por chamadas diretas a instrumentos colocaria um SDK de
telemetria dentro da regra de negócio.

A segunda é de cardinalidade, e neste projeto ela é um problema de segurança
antes de ser um problema de custo. Um identificador de evento, uma chave de
idempotência ou um `correlation_id` como label multiplicaria séries sem limite.
Pior: o texto de um erro de driver pode carregar a DSN do PostgreSQL com senha,
e o de um erro de AMQP pode carregar a URL do broker com credenciais — é
exatamente a superfície que o [ADR 0017](0017-sensitive-data-and-error-handling.md)
identificou em `last_error`. Um label é enviado para um backend externo, é
indexado e frequentemente é público dentro da organização.

A terceira é de bloqueio. Profundidade de fila e backlog são estado, não
eventos, então a forma natural seria um instrumento observável com callback. Só
que um callback de coleta roda na thread do exportador: ler a profundidade de
uma fila ali significa abrir um canal AMQP dentro dele, e um broker que parou
de responder passaria a travar a exportação inteira. Há ainda um detalhe do
protocolo: uma declaração passiva de fila inexistente **fecha o canal** em que
rodou, o que jamais pode acontecer com o canal de consumo em produção.

## Decisão

### `internal/platform/metrics` concentra instrumentos, observers e normalização

O pacote é o único lugar do repositório que importa a API de métricas do
OpenTelemetry fora de `internal/platform/telemetry`. Ele implementa as
interfaces de observer que as outras camadas já declaravam, no vocabulário
delas, e traduz o que ouve em instrumentos. As composições em
`internal/runtime/api` e `internal/runtime/worker` ligam as duas pontas.

Nenhuma interface de observer existente foi substituída. Onde faltava
informação para uma métrica honesta, a interface foi **estendida por uma
segunda interface opcional**, que o observer de métricas implementa e o de
logging não:

- `outbox.CycleObserver` acrescenta `PublicationSettled` e `CycleCompleted`.
  A interface original só reportava exceções — mensagem presa, abandonada,
  lease perdido — e uma métrica precisa também dos sucessos.
- `consumer.HandlingObserver` acrescenta `Handled`, com duração, disposição e
  as transições efetivamente commitadas.
- `rabbitmq.DeliveryObserver` acrescenta `Redelivered`.

O logger continua recebendo apenas o que era ruidoso demais para virar log de
cada mensagem. `WithObserver` passou a aceitar mais de um observer, então
logging e métricas convivem sem que um saiba do outro.

Os casos de uso de webhook, checkout e pedidos não tinham observer nenhum e
ganharam um. `webhooks.Receipt`, `payments.ProviderCall` e os dois métodos de
idempotência de `orders` foram desenhados para carregar apenas o que é seguro
agregar: nenhum deles inclui identificador, corpo ou texto de erro.

### A allowlist de labels é explícita e fechada

Toda atribuição passa por `Normalize(chave, valor)`. Uma chave desconhecida, um
valor desconhecido e um valor vazio viram `other`. A lista de valores permitidos
é escrita à mão e **não é derivada dos enums do domínio**: um novo tipo de
evento da Stripe ou um novo estado de pagamento tem que ser uma decisão
registrada aqui, não uma série nova que aparece porque um enum cresceu.

Labels permitidas:

| Chave | Valores |
| --- | --- |
| `provider` | `stripe` |
| `event.kind` | `checkout.completed`, `checkout.payment_succeeded`, `checkout.payment_failed`, `checkout.expired`; qualquer tipo desconhecido vira `other` |
| `outcome` | `accepted`, `duplicate`, `ignored`, `success`, `error`, `published`, `retrying`, `failed`, `lease_lost`, `empty` |
| `reason` | `invalid_signature`, `storage`, `internal`, `not_confirmed`, `not_routed`, `permanent`, `transient`, `lease_lost`, `already_processed`, `stale_event`, `incompatible_api_version`, `unreadable_payload`, `missing_reference`, `aggregate_not_found`, `reference_mismatch`, `amount_mismatch`, `state_conflict`, `invariant_violated`, `unclassified` |
| `operation` | `create_order`, `create_checkout` |
| `entity` | `order`, `payment`, `attempt` |
| `from`, `to` | `pending`, `processing`, `succeeded`, `failed`, `cancelled`, `expired`, `partially_refunded`, `refunded`, `created`, `paid` |
| `stage` | `lease`, `settlement` |
| `disposition` | `done`, `retry`, `dead`, `unrecorded` |
| `destination` | `retry`, `dead_letter` |
| `state` | `in_use`, `idle` |
| `sampler` | `backlog`, `broker` |
| `queue` | os nomes da topologia declarada pelo processo |

`queue` é a única chave sem lista estática, porque seus valores são os nomes de
fila que o próprio processo declarou: fila principal, dead-letter e uma por
faixa de retry configurada. O observer e o sampler recebem esse conjunto na
construção e colapsam qualquer outro nome em `other`, de modo que uma fila
renomeada aparece como anomalia em vez de criar uma série.

Proibidos como label, sem exceção: identificadores de pedido, pagamento,
tentativa, evento e mensagem; `correlation_id`; chaves de idempotência;
segredos e credenciais; texto de mensagem de erro; caminho HTTP livre; e tipo de
evento do provedor não normalizado. Um erro classificado vira `reason`; o erro
em si permanece em log e trace, que têm controle de acesso próprio.

### Instrumentos

Todos são criados a partir de um catálogo único, em `catalogue.go`. Nome,
descrição, unidade, tipo e labels vivem lá, e um teste compara o que um
`ManualReader` coleta com o que o catálogo declara: um instrumento não pode
existir sem estar descrito.

| Instrumento | Tipo | Unidade | Labels |
| --- | --- | --- | --- |
| `payment.webhook.received` | counter | `{event}` | `provider`, `event.kind`, `outcome` |
| `payment.webhook.invalid_signature` | counter | `{request}` | `provider` |
| `payment.webhook.duplicate` | counter | `{event}` | `provider`, `event.kind` |
| `payment.webhook.receive.duration` | histogram | `s` | `provider`, `outcome` |
| `payment.webhook.receive.failures` | counter | `{request}` | `provider`, `reason` |
| `payment.provider.request.duration` | histogram | `s` | `provider`, `operation`, `outcome` |
| `payment.provider.request.failures` | counter | `{request}` | `provider`, `operation`, `outcome` |
| `payment.idempotency.replays` | counter | `{request}` | `operation` |
| `payment.idempotency.conflicts` | counter | `{request}` | `operation` |
| `payment.state.transitions` | counter | `{transition}` | `entity`, `from`, `to` |
| `outbox.publish.duration` | histogram | `s` | `outcome` |
| `outbox.publish.failures` | counter | `{message}` | `reason` |
| `outbox.relay.cycle.duration` | histogram | `s` | `outcome` |
| `outbox.relay.cycle.failures` | counter | `{cycle}` | `stage` |
| `outbox.relay.stuck` | counter | `{message}` | — |
| `outbox.relay.abandoned` | counter | `{message}` | — |
| `outbox.relay.lease_lost` | counter | `{message}` | — |
| `payment.consumer.handle.duration` | histogram | `s` | `disposition` |
| `payment.consumer.no_ops` | counter | `{event}` | `reason` |
| `payment.consumer.failures` | counter | `{event}` | `reason` |
| `payment.consumer.retries` | counter | `{event}` | — |
| `payment.consumer.dead_letters` | counter | `{event}` | `reason` |
| `rabbitmq.consumer.redeliveries` | counter | `{message}` | — |
| `rabbitmq.consumer.republished` | counter | `{message}` | `destination`, `queue` |
| `rabbitmq.consumer.rejected` | counter | `{message}` | — |
| `rabbitmq.consumer.unacknowledged` | counter | `{message}` | — |
| `outbox.pending` | gauge | `{message}` | — |
| `outbox.oldest.age` | gauge | `s` | — |
| `webhook.inbox.pending` | gauge | `{event}` | — |
| `webhook.inbox.failed` | gauge | `{event}` | — |
| `webhook.inbox.oldest.age` | gauge | `s` | — |
| `rabbitmq.queue.depth` | gauge | `{message}` | `queue` |
| `database.pool.connections` | gauge | `{connection}` | `state` |
| `database.pool.max_open` | gauge | `{connection}` | — |
| `database.pool.waits` | observable counter | `{wait}` | — |
| `metrics.sampler.age` | gauge | `s` | `sampler` |
| `metrics.sampler.failures` | observable counter | `{sample}` | `sampler` |

A duração de requisição HTTP continua vindo do `otelhttp` e não é
reimplementada aqui.

### `pending` inclui trabalho em andamento

`outbox.pending` conta linhas em `pending` **e** em `publishing`. Uma mensagem
cujo lease foi tomado e cujo desfecho ainda não foi gravado continua sendo
trabalho por fazer; excluí-la faria um relay travado parecer um outbox vazio.
`webhook.inbox.pending` conta `pending` e `processing` pela mesma razão.

`rabbitmq.queue.depth` conta apenas mensagens prontas. Mensagens já entregues e
não confirmadas pertencem a um consumer e não estão esperando na fila.

As duas queries de backlog já existiam em `db/queries` e foram reutilizadas
como estão. Nenhuma query nova foi escrita para métricas.

### Estado é amostrado em background, nunca dentro do callback de coleta

Um `Sampler` roda o coletor em goroutine própria, com timeout por rodada, e os
gauges publicam o último valor que a rodada conseguiu coletar. Uma rodada que
falha não muda nada: os gauges permanecem no último valor conhecido,
`metrics.sampler.failures` incrementa e `metrics.sampler.age` cresce até uma
rodada voltar a funcionar. Um banco ou um broker indisponível produz um sinal
visível, e não uma coleta bloqueada, uma série que some ou um processo que cai.
`Sampler.Run` retorna `nil` sempre: falha de observação não derruba o trabalho
observado.

O sampler de fila usa **conexão e canal próprios**, a `payments-worker-metrics`.
São duas razões independentes, ambas suficientes: a declaração passiva de uma
fila inexistente fecha o canal em que rodou, e o timeout do sampler instala um
prazo no socket que interromperia uma entrega ou uma republicação em curso.

O pool do PostgreSQL é a exceção deliberada: `sql.DB.Stats()` é uma cópia sob
lock em memória, sem I/O, então é lido em callback observável sem sampler.

`METRICS_SAMPLE_INTERVAL` e `METRICS_SAMPLE_TIMEOUT` configuram a coleta. A
configuração recusa um timeout maior ou igual ao intervalo, porque isso
permitiria que uma rodada lenta ainda estivesse rodando quando a próxima
dispara, transformando uma dependência travada em amostragem concorrente
ilimitada em vez de uma lacuna visível.

### Desabilitar OTLP não muda comportamento

Com `OTEL_ENABLED=false`, `telemetry.New` não instala MeterProvider e o global
permanece o no-op. Os instrumentos são criados do mesmo jeito, os observers são
ligados do mesmo jeito e os samplers rodam do mesmo jeito; apenas nada é
registrado. Nenhum caminho de código da aplicação pergunta se métricas estão
habilitadas.

## Alternativas consideradas

### Chamar instrumentos diretamente do caso de uso

Rejeitada. Colocaria OpenTelemetry dentro da regra de negócio e tornaria os
testes de caso de uso dependentes de um SDK de telemetria. A interface de
observer já existia justamente para isso.

### Derivar a allowlist dos enums do domínio

Rejeitada. Seria menos código e exatamente o comportamento errado: um enum que
cresce passaria a criar séries novas sem revisão. A lista explícita torna o
crescimento da cardinalidade uma decisão.

### Truncar valores de label em vez de normalizar

Rejeitada, pelo mesmo motivo pelo qual truncar `last_error` não sanitiza. Os
primeiros cinquenta caracteres de uma DSN contêm a senha, e um identificador
truncado continua sendo alta cardinalidade.

### Ler profundidade de fila em callback observável

Rejeitada. É a forma canônica para gauges, mas exigiria I/O de rede na thread do
exportador e uso do canal de produção. Um broker lento passaria a atrasar toda
a exportação, e uma fila ausente fecharia o canal de consumo.

### Um `pkg/metrics` público desde já

Adiada. O ADR 0001 reserva extração de fronteiras reutilizáveis para depois de
uma implementação estável. O pacote é `internal` até lá.

## Consequências

### Positivas

- Backlog, DLQ, retries e falhas do provedor passam a ser observáveis sem
  consulta manual ao banco ou ao broker.
- Nenhum identificador, segredo ou texto de erro pode virar label, e um teste
  falha quando alguém tenta.
- Indisponibilidade de banco ou broker fica visível pela idade da coleta em vez
  de bloquear a exportação.
- O dashboard versionado é verificado contra o catálogo e contra a allowlist na
  CI, então uma consulta a instrumento inexistente não passa despercebida.

### Limitações

- A allowlist tem custo de manutenção: um provedor novo ou um tipo de evento
  novo exige mudança de código e deste ADR. É o custo aceito para que
  cardinalidade seja decisão e não acidente.
- Gauges de backlog são amostrados, então têm a granularidade do intervalo e
  não capturam picos entre rodadas. Para picos, os contadores são a fonte.
- `rabbitmq.queue.depth` só existe onde o worker roda; a API não abre conexão
  com o broker.
- Múltiplas instâncias de worker amostram o mesmo backlog independentemente. Um
  painel precisa agregar com `max`, não com `sum`, o que o dashboard versionado
  já faz.

## Referências

- [ADR 0008: Zap e OpenTelemetry](0008-observability-stack.md)
- [ADR 0011: recepção de webhook e outbox](0011-webhook-reception-and-outbox.md)
- [ADR 0012: relay do outbox e topologia](0012-outbox-relay-and-topology.md)
- [ADR 0013: transações, transições e retry do consumer](0013-consumer-transactions-transitions-and-retry.md)
- [ADR 0017: dados sensíveis, retenção e superfície de erro](0017-sensitive-data-and-error-handling.md)
