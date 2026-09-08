# Métricas

Este documento tem duas partes que não devem ser confundidas. A primeira
descreve os instrumentos que a aplicação **publica hoje**: cada nome abaixo
existe no código e é verificado por teste. A segunda registra referências
operacionais e metas de produto, que são alvos e não garantias.

A definição normativa de nome, tipo, unidade e labels está no
[ADR 0015](decisions/0015-application-metrics-and-cardinality.md). Este
documento explica como usá-los.

## Como habilitar

A exportação é OTLP e opcional:

```bash
OTEL_ENABLED=true
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
OTEL_EXPORT_INTERVAL=10s
METRICS_SAMPLE_INTERVAL=15s
METRICS_SAMPLE_TIMEOUT=5s
```

Com `OTEL_ENABLED=false` nada é exportado e o comportamento funcional é
idêntico: os instrumentos são criados contra o provider no-op, e nenhum caminho
da aplicação pergunta se métricas estão habilitadas.

Para o ambiente local completo, com Grafana e o dashboard já provisionado:

```bash
make observability-up
```

## Instrumentos publicados

Nenhum instrumento usa identificador, chave de idempotência, `correlation_id`,
segredo, texto de erro ou caminho HTTP livre como label. Valores fora da lista
permitida viram `other`. Um teste falha se uma instrumentação violar isso.

### HTTP

| Instrumento | Tipo | Unidade | Labels | O que mede |
| --- | --- | --- | --- | --- |
| `http.server.request.duration` | histogram | `s` | — | Duração das requisições, pelo `otelhttp`. |
| `http.server.rate_limited` | counter | `{request}` | `limiter`, `route.class` | Requisições recusadas com `429`, por limitador e classe de rota. |

`limiter` diz qual balde se esgotou: `client` (grosseiro, por endereço, para
negócio, operações, documentação e caminhos desconhecidos),
`credential` (por credencial válida), `webhook` (balde global do provedor) ou
`health` (balde próprio da probe).
`route.class` agrupa as operações em `health`, `docs`, `business`, `operations`
e `webhook`. Os dois são vocabulários fechados: endereço, impressão da
credencial, chave de API e caminho HTTP nunca viram label, então o número de
séries é fixo e não cresce com o número de chamadores. A política está no
[ADR 0018](decisions/0018-rate-limiting.md).

Um `client` subindo concentrado é abuso ou um cliente mal comportado; um
`credential` subindo costuma ser um laço no backend do integrador, e é o sinal
que chega antes de a cota da Stripe acabar. `webhook` acima de zero merece
atenção imediata: a Stripe está sendo recusada e vai reentregar. `health` acima
de zero significa que a probe está sendo recusada, e uma réplica saudável pode
sair de rotação por isso.

### Recepção de webhook

| Instrumento | Tipo | Unidade | Labels | O que mede |
| --- | --- | --- | --- | --- |
| `payment.webhook.received` | counter | `{event}` | `provider`, `event.kind`, `outcome` | Eventos recebidos, por desfecho: `accepted`, `duplicate` ou `ignored`. |
| `payment.webhook.invalid_signature` | counter | `{request}` | `provider` | Requisições recusadas porque a assinatura não pôde ser verificada. |
| `payment.webhook.duplicate` | counter | `{event}` | `provider`, `event.kind` | Reentregas de um evento já armazenado. |
| `payment.webhook.receive.duration` | histogram | `s` | `provider`, `outcome` | Tempo entre receber a requisição e ter o evento durável na inbox. |
| `payment.webhook.receive.failures` | counter | `{request}` | `provider`, `reason` | Requisições que terminaram sem evento armazenado. |

Um `duplicate` é o comportamento correto de uma reentrega, não um erro. Uma
assinatura inválida sustentada quase sempre indica segredo de endpoint errado:
a Stripe reentrega e toda tentativa falha do mesmo jeito até a configuração ser
corrigida.

### Provedor de pagamento e idempotência

| Instrumento | Tipo | Unidade | Labels | O que mede |
| --- | --- | --- | --- | --- |
| `payment.provider.request.duration` | histogram | `s` | `provider`, `operation`, `outcome` | Latência de uma chamada ao provedor. |
| `payment.provider.request.failures` | counter | `{request}` | `provider`, `operation`, `outcome` | Chamadas ao provedor que falharam. |
| `payment.idempotency.replays` | counter | `{request}` | `operation` | Requisições respondidas a partir de um resultado anterior. |
| `payment.idempotency.conflicts` | counter | `{request}` | `operation` | Chave de idempotência reusada com payload diferente. |

A latência do provedor é medida separadamente da latência interna porque não
está sob controle da aplicação. Um replay é esperado; um conflito é erro de
integração e responde `409`.

### Domínio

| Instrumento | Tipo | Unidade | Labels | O que mede |
| --- | --- | --- | --- | --- |
| `payment.state.transitions` | counter | `{transition}` | `entity`, `from`, `to` | Transições efetivamente gravadas pelo consumer. |

Somente transições commitadas contam. Um evento que a matriz decidiu ignorar
aparece em `payment.consumer.no_ops`, não aqui.

### Relay do outbox

| Instrumento | Tipo | Unidade | Labels | O que mede |
| --- | --- | --- | --- | --- |
| `outbox.publish.duration` | histogram | `s` | `outcome` | Tempo até o broker confirmar ou recusar uma mensagem. |
| `outbox.publish.failures` | counter | `{message}` | `reason` | Publicações que não terminaram confirmadas. |
| `outbox.relay.cycle.duration` | histogram | `s` | `outcome` | Duração de um ciclo: reservar lote, publicar e liquidar. |
| `outbox.relay.cycle.failures` | counter | `{cycle}` | `stage` | Ciclos que falharam, por etapa. |
| `outbox.relay.stuck` | counter | `{message}` | — | Mensagens que passaram do limite de alerta e continuam tentando. |
| `outbox.relay.abandoned` | counter | `{message}` | — | Mensagens abandonadas por falha permanente. |
| `outbox.relay.lease_lost` | counter | `{message}` | — | Desfechos que não puderam ser gravados a tempo. |

Um ciclo com desfecho `empty` é o relay ocioso, o estado normal. Uma falha na
etapa `lease` aponta o PostgreSQL; na etapa `settlement`, a gravação do
desfecho depois de o broker já ter aceito a mensagem.

`outbox.relay.abandoned` acima de zero merece atenção imediata: apenas um erro
classificado como permanente chega ali, e indisponibilidade do broker nunca é
permanente.

### Consumer, retry e DLQ

| Instrumento | Tipo | Unidade | Labels | O que mede |
| --- | --- | --- | --- | --- |
| `payment.consumer.handle.duration` | histogram | `s` | `disposition` | Tempo para aplicar um evento entregue. |
| `payment.consumer.no_ops` | counter | `{event}` | `reason` | Eventos que não mudaram nada. |
| `payment.consumer.failures` | counter | `{event}` | `reason` | Eventos cuja aplicação falhou. |
| `payment.consumer.retries` | counter | `{event}` | — | Eventos reagendados para nova tentativa. |
| `payment.consumer.dead_letters` | counter | `{event}` | `reason` | Eventos enviados para a dead-letter queue. |
| `rabbitmq.consumer.redeliveries` | counter | `{message}` | — | Entregas que o broker marcou como reentrega. |
| `rabbitmq.consumer.republished` | counter | `{message}` | `destination`, `queue` | Cópias confirmadas enviadas a um tier de retry ou à DLQ. |
| `rabbitmq.consumer.rejected` | counter | `{message}` | — | Mensagens ilegíveis enviadas direto à DLQ. |
| `rabbitmq.consumer.unacknowledged` | counter | `{message}` | — | Mensagens deixadas sem ack para reentrega. |

Os dois motivos de no-op são diferentes e importam: `already_processed` é uma
reentrega chegando depois do efeito commitado, e `stale_event` é a matriz de
transições recusando um evento fora de ordem. Nenhum dos dois é erro.

A disposição `unrecorded` no histograma é o caso em que nem o desfecho pôde ser
gravado: a mensagem fica sem ack e o broker a reentrega.

### Estado: backlog, filas e pool

Estes são gauges alimentados por samplers em background, não por callback de
coleta. Detalhe do desenho e do porquê no ADR 0015.

| Instrumento | Tipo | Unidade | Labels | O que mede |
| --- | --- | --- | --- | --- |
| `outbox.pending` | gauge | `{message}` | — | Mensagens não publicadas, incluindo as em publicação. |
| `outbox.oldest.age` | gauge | `s` | — | Idade da mensagem não publicada mais antiga. |
| `webhook.inbox.pending` | gauge | `{event}` | — | Eventos pendentes ou em processamento. |
| `webhook.inbox.failed` | gauge | `{event}` | — | Eventos com falha aguardando reprocessamento. |
| `webhook.inbox.oldest.age` | gauge | `s` | — | Idade do evento não aplicado mais antigo. |
| `rabbitmq.queue.depth` | gauge | `{message}` | `queue` | Mensagens prontas em cada fila da topologia declarada. |
| `database.pool.connections` | gauge | `{connection}` | `state` | Conexões PostgreSQL em uso e ociosas. |
| `database.pool.max_open` | gauge | `{connection}` | — | Teto configurado de conexões abertas. |
| `database.pool.waits` | observable counter | `{wait}` | — | Vezes em que um chamador esperou por conexão. |
| `metrics.sampler.age` | gauge | `s` | `sampler` | Segundos desde a última coleta bem-sucedida. |
| `metrics.sampler.failures` | observable counter | `{sample}` | `sampler` | Rodadas de coleta que falharam ou estouraram o timeout. |

`pending` inclui trabalho em andamento: uma mensagem cujo lease foi tomado e
cujo desfecho não foi gravado continua sendo trabalho por fazer. `queue.depth`
conta apenas mensagens prontas; as já entregues e não confirmadas pertencem a
um consumer.

**Leia sempre `metrics.sampler.age` junto dos gauges acima.** Quando uma coleta
falha, os gauges mantêm o último valor conhecido em vez de zerar ou sumir. Uma
idade que cresce indefinidamente significa que os valores estão congelados, não
que o backlog está estável.

Com mais de uma instância de worker, agregue backlog e profundidade com `max`,
nunca com `sum`: todas amostram o mesmo estado compartilhado.

## Nomes no Prometheus

A tradução de OTLP para Prometheus renomeia os instrumentos: pontos viram
underscore, a unidade `s` acrescenta `_seconds` e contadores ganham `_total`.
Um histograma vira três séries, `_bucket`, `_count` e `_sum`.

```promql
# DLQ acima de zero: sempre acionável.
max by (queue) (rabbitmq_queue_depth{queue="payments.webhooks.dlq"})

# Outbox envelhecendo: o relay não está publicando.
max(outbox_oldest_age_seconds)

# Aceite durável do webhook, p95.
histogram_quantile(0.95, sum by (le) (rate(payment_webhook_receive_duration_seconds_bucket[5m])))

# Gauges de backlog congelados por falha de coleta.
max by (sampler) (metrics_sampler_age_seconds)
```

## Dashboard

O dashboard versionado está em
[`deployments/observability/payments-pipeline.json`](../deployments/observability/payments-pipeline.json)
e cobre HTTP, provedor, inbox, outbox, retry, DLQ e pool do PostgreSQL. Ele é
provisionado automaticamente pelo profile `observability` do Compose.

Um teste na CI verifica que toda consulta do dashboard nomeia um instrumento
que a aplicação publica e nenhuma label fora da allowlist, de modo que um painel
permanentemente vazio é detectado antes de um incidente.

## Referências operacionais iniciais

Os valores abaixo **não são SLA**. São pontos de partida para alertas, medidos
em ambiente de desenvolvimento, e serão revisados com dados de produção na
entrega E9.

| Sinal | Referência inicial | Ação sugerida |
| --- | ---: | --- |
| `rabbitmq.queue.depth` na DLQ | acima de 0 | Runbook da DLQ; nada chega ali sem ter esgotado tentativas ou falhado de forma terminal. |
| `outbox.oldest.age` | acima de 60 s | Verificar broker e relay. |
| `webhook.inbox.failed` | acima de 0 | Inspecionar por `GET /v1/webhook-events?status=failed` e reprocessar. |
| `metrics.sampler.age` | acima de 60 s | Gauges de estado estão desatualizados; verificar banco e broker. |
| `outbox.relay.abandoned` | acima de 0 | Falha permanente; exige diagnóstico, não espera. |
| Aceite durável do webhook, p95 | abaixo de 250 ms | — |
| Processamento completo do evento, p95 | abaixo de 5 s | — |
| Processamento completo do evento, p99 | abaixo de 30 s | — |

## Metas de produto

Estas métricas são de adoção e de correção. Não são instrumentos exportados; são
compromissos verificados por testes e por revisão.

### Experiência de adoção

| Métrica | Referência inicial |
| --- | ---: |
| API disponível após clone e configuração | até 10 minutos |
| Primeiro pagamento em sandbox | até 20 minutos |
| Endpoints públicos presentes no OpenAPI | 100% |
| Preparação manual do banco e do broker | nenhuma |
| Exemplos documentados verificados em CI | 100% |

`Time to First Successful Payment` é a principal métrica de utilidade: mede o
tempo entre obter o repositório e confirmar o primeiro pagamento em sandbox.

### Correção

- Cobranças duplicadas causadas pela aplicação: zero.
- Efeitos duplicados após redelivery: zero.
- Eventos válidos perdidos em testes de falha: zero.
- Transições inválidas ou regressivas: zero.
- Divergências silenciosas entre pedido e pagamento: zero.

Não é prometida entrega exatamente uma vez. A arquitetura assume entrega pelo
menos uma vez e garante idempotência dos efeitos observáveis.

## Testes de desempenho

Cada resultado publicado deve informar máquina, versão do Go, PostgreSQL,
RabbitMQ, configuração e cenário. Não são usados benchmarks isolados de
framework para afirmar capacidade do sistema completo.

Os primeiros testes devem avaliar:

- Criação e consulta concorrente de pedidos.
- Rajada de webhooks válidos, inválidos e duplicados.
- Crescimento e recuperação do outbox durante indisponibilidade do RabbitMQ.
- Redelivery após interrupção do worker.
- Tempo para drenar backlog com diferentes valores de concorrência e prefetch.
