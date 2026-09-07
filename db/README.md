# Banco de dados

`migrations/` contem migrations SQL executadas pelo Goose. Migrations publicadas
sao imutaveis. `queries/` contem SQL nomeado para geracao com `sqlc`.

GORM nao executa `AutoMigrate`. O schema nasce exclusivamente das migrations.

O schema e interno ao projeto. Integradores consomem a API HTTP, nunca as
tabelas diretamente.

## Modelagem

```text
                    +--------------------------------+
                    |             orders             |
                    |--------------------------------|
                    | id                         PK  |
                    | status                         |  pending | paid
                    | amount / currency              |  cancelled | expired
                    | (id, amount, currency)      U*  |
                    | product_id / quantity          |
                    | idempotency_key             U  |
                    +--------------------------------+
                                    |
                          1:N       |  um pedido acumula cobrancas ao longo
                                    |  do tempo, mas no maximo uma delas pode
                                    |  estar viva ou liquidada
                                    |
                                    |  FK composta (order_id, amount, currency)
                                    v  prende o valor dos dois lados
                    +--------------------------------+
                    |            payments            |
                    |--------------------------------|
                    | id                         PK  |
                    | order_id                   FK  |  pending | processing
                    | provider                       |  succeeded | failed
                    | (id, provider)             U*  |
                    | status                         |  cancelled
                    | amount / currency          FK  |  partially_refunded
                    +--------------------------------+  refunded
                                    |
                          1:N       |  cada chamada feita ao provedor
                                    |
                                    |  FK composta (payment_id, provider)
                                    v  impede tentativa em outro provedor
                    +--------------------------------+
                    |        payment_attempts        |
                    |--------------------------------|
                    | id                         PK  |
                    | payment_id / provider      FK  |  created | pending
                    | status                         |  succeeded | failed
                    | idempotency_key             U  |  expired | cancelled
                    | provider_session_id        U*  |
                    | provider_payment_intent_id     |
                    | checkout_url / expires_at      |
                    | failure_code / failure_message |
                    +--------------------------------+
                                    ^
                                    |  correlacao por provider_session_id,
                                    |  resolvida em tempo de processamento
                                    |
                    +--------------------------------+
                    |         webhook_events         |
                    |--------------------------------|
                    | id                         PK  |
                    | provider/provider_event_id  U* |  pending | processing
                    | event_type                     |  processed | failed
                    | raw_payload (bytea)            |  skipped
                    | payload (jsonb)                |
                    | status / attempts / last_error |
                    | received_at / processed_at     |
                    +--------------------------------+

                    PK  chave primaria
                    FK  chave estrangeira
                    U   unicidade de uma coluna
                    U*  unicidade composta, detalhada nas invariantes abaixo
```

## Entidades

### orders

A intencao comercial. Guarda o que foi pedido (`product_id`, `quantity`) e o
que o servidor calculou a partir disso (`amount`, `currency`). O cliente nunca
envia o valor a ser cobrado.

O `status` usa vocabulario comercial e nao financeiro. Um cartao recusado nao
torna o pedido invalido: ele continua `pending`, aguardando pagamento. Somente
`paid`, `cancelled` e `expired` encerram o pedido.

### payments

O estado financeiro consolidado de uma cobranca. `amount` e `currency` sao
copiados do pedido no momento em que a cobranca nasce, e a copia e amarrada ao
pedido por uma chave estrangeira composta: nao pode nascer diferente do pedido,
nao pode ser alterada depois, e o pedido tampouco pode ser reprecificado
enquanto a cobranca existir.

O `status` carrega a maquina de estados financeira completa, incluindo os
estados de reembolso, reservados para evolucao posterior.

### payment_attempts

Cada chamada individual feita ao provedor sob uma cobranca. Registra a chave de
idempotencia enviada remotamente e os identificadores que voltaram de la
(`provider_session_id`, `provider_payment_intent_id`), alem da URL de checkout,
sua expiracao e o motivo da falha quando houver.

O `provider` da tentativa e amarrado ao do pagamento por chave estrangeira
composta, entao uma cobranca da Stripe nao pode acumular tentativas em outro
provedor.

Separar tentativa de cobranca existe porque a chamada remota pode falhar,
repetir ou expirar sem que o estado financeiro mude. A tentativa e o registro de
dialogo com o provedor; o pagamento e a conclusao.

### webhook_events

O schema do inbox duravel dos eventos recebidos ja existe, mas nenhuma linha e
gravada pelo runtime atual. Na Fase 3, a linha sera inserida dentro da requisicao
do webhook, antes de qualquer efeito de negocio, e so depois disso a API
respondera sucesso ao provedor.

O evento sera guardado em duas formas. `raw_payload` preservara os bytes exatos
que o provedor assinou, pois `jsonb` reordena chaves e descarta formatacao, e
esses bytes desaparecem junto com a requisicao. `payload` guardara o mesmo
evento desserializado, para consulta e reprocessamento.

`attempts` e `last_error` sustentarao retry com backoff e diagnostico do que
parou na dead-letter queue.

## Relacionamentos

| Origem | Destino | Cardinalidade | Como |
| --- | --- | --- | --- |
| `orders` | `payments` | 1:N | FK composta `(order_id, amount, currency)` |
| `payments` | `payment_attempts` | 1:N | FK composta `(payment_id, provider)` |
| `webhook_events` | `payment_attempts` | N:1 logico | `provider_session_id` extraido do payload |

A ligacao entre `webhook_events` e o restante do modelo e deliberadamente uma
correlacao, e nao uma chave estrangeira. Um evento pode chegar antes do commit
da tentativa que ele descreve, pode se referir a um objeto que o projeto ainda
nao modela, ou pode simplesmente nao interessar. Uma chave estrangeira faria a
gravacao do inbox falhar em todos esses casos, justamente quando a garantia
desejada e a oposta: aceitar o evento de forma duravel primeiro e decidir o que
fazer com ele depois.

## Invariantes garantidas pelo schema

O banco recusa a escrita nos casos abaixo, sem depender da aplicacao.

| Indice ou constraint | O que impede |
| --- | --- |
| `orders_idempotency_key_key` | Criar dois pedidos com a mesma `Idempotency-Key`. |
| `payments_order_id_active_key` | Abrir uma cobranca ao lado de outra viva ou ja liquidada. |
| `payment_attempts_idempotency_key_key` | Abrir duas sessoes para a mesma chave de checkout. |
| `payment_attempts_payment_id_active_key` | Abrir duas sessoes para o mesmo pagamento sob chaves diferentes. |
| `payments_order_fkey` | Cobrar valor ou moeda diferente do pedido, ou reprecificar o pedido depois. |
| `payment_attempts_payment_fkey` | Uma tentativa apontar para provedor diferente do pagamento. |
| `payment_attempts_provider_session_id_key` | Uma sessao do provedor resolver para mais de uma tentativa. |
| `webhook_events_provider_event_id_key` | Persistir duas linhas para o mesmo evento em uma redelivery. |
| `orders_amount_check`, `payments_amount_check` | Cobrancas de valor zero ou negativo. |
| `orders_currency_check`, `payments_currency_check` | Moeda fora do formato ISO 4217. |
| `*_status_check` | Estados fora da maquina de cada entidade. |

`payments_order_id_active_key` e a barreira contra cobranca duplicada e merece
detalhe. O indice e unico por `order_id`, mas parcial: vale apenas onde o status
nao e `failed` nem `cancelled`. Cobrancas mortas saem do indice e viram
historico, entao um retry e sempre possivel. Qualquer outro estado ocupa a vaga
unica do pedido.

No runtime da Fase 2, uma falha ambigua no provedor mantem a tentativa ativa e
deve ser retomada com a mesma `Idempotency-Key`. As transicoes que liberam uma
nova tentativa entram na Fase 3.

O efeito colateral e que um webhook fora de ordem nao consegue reviver uma
cobranca antiga enquanto existir uma liquidada, porque a transicao esbarra no
mesmo indice. A protecao contra regressao de estado nasce como constraint, e nao
como condicional no worker.

`payment_attempts_payment_id_active_key` aplica a mesma forma um nivel abaixo.
Duas requisicoes concorrentes de checkout com chaves de idempotencia diferentes
abririam duas sessoes no provedor para a mesma cobranca, e o cliente poderia
pagar as duas. O indice permite apenas uma tentativa aberta ou liquidada por
pagamento. Em troca, a aplicacao precisa marcar a tentativa anterior como
`expired`, `failed` ou `cancelled` antes de abrir a proxima.

## Fluxo de escrita de um pagamento

```text
POST /v1/orders
  INSERT orders                      status pending

POST /v1/orders/{orderId}/checkout
  BEGIN
    INSERT payments                  status pending
    INSERT payment_attempts          status created
  COMMIT
  -> cria a sessao no provedor
  UPDATE payment_attempts            status pending, guarda sessao, URL e expiracao

Fase 3: webhook do provedor
  BEGIN
    INSERT webhook_events            status pending, dentro da requisicao
    INSERT outbox_events             mesma transacao
  COMMIT
  -> resposta 2xx ao provedor

Fase 3: worker
  UPDATE payment_attempts            resultado da tentativa
  UPDATE payments                    estado financeiro consolidado
  UPDATE orders                      status paid, na mesma transacao
  UPDATE webhook_events              status processed
```

Na Fase 3, as quatro atualizacoes do worker ocorrerao em uma unica transacao. E
isso que impedira o pedido de dizer `paid` enquanto a cobranca ainda estiver
`processing`.

## Ainda nao modelado

- `outbox_events`, introduzida na Fase 3 junto com o relay.
- `refunds`, que hoje existe apenas como estado em `payments.status`. O schema
  sabe que houve reembolso, mas nao quanto nem quantos.
- `customers`, fora do escopo da versao 0.1.
