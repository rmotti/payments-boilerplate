# Arquitetura

Este documento descreve as fronteiras e invariantes da API em Go e distingue o
runtime atual da arquitetura-alvo da versão `0.1.0`. A API usa PostgreSQL como
fonte de verdade, abre Stripe Checkout e já recebe webhooks assinados,
gravando o evento e sua mensagem de outbox na mesma transação. O relay que
publica essas mensagens e o consumer que as aplica rodam dentro do processo
`worker`. A API também expõe inspeção operacional autenticada e replay seguro;
o Checkout oferece cartão e Pix em BRL.

## Contexto

O diagrama representa a arquitetura-alvo da versão `0.1.0`:

```text
Swagger UI ou sistema do integrador
                 |
                 v
          Payments API --------------------> Provedor
                 ^                              |
                 |                              | webhook
                 |                              v
                 +------------------------- Payments API
                                                |
                                       transação inbox + outbox
                                                |
                                                v
                                           PostgreSQL
                                                |
                                          Outbox relay
                                                |
                                                v
                                            RabbitMQ
                                                |
                                                v
                                       Payments Worker
                                                |
                                                v
                                           PostgreSQL
```

O checkout coleta os dados de pagamento em uma página hospedada pela Stripe.
A API cria e consulta suas próprias entidades de negócio e já recebe o
resultado assíncrono por um endpoint de webhook. O Swagger UI documenta e
exercita o contrato, mas não faz parte do fluxo de produção de quem adotar o
projeto.

A API persiste o webhook e uma mensagem de outbox na mesma transação. O relay
publica a mensagem no RabbitMQ, e o consumer aplica seus efeitos de forma
idempotente, em uma única transação que bloqueia o agregado inteiro. API, relay
e consumer pertencem ao mesmo código-base.

## Responsabilidades

### Payments API

- Criar o pedido e calcular seu valor no servidor.
- Receber, validar e persistir a chave de idempotência escolhida pelo integrador.
- Solicitar a criação do checkout ao provedor.
- Associar identificadores locais aos identificadores externos.
- Validar e armazenar eventos recebidos do provedor.
- Criar a mensagem de outbox na mesma transação do evento.
- Expor ao sistema integrador o estado conhecido pela API.
- Expor metadados não sensíveis da inbox/outbox e reenfileirar atomicamente
  apenas trabalho que falhou.
- Publicar um contrato OpenAPI coerente com a implementação.

### Outbox relay

Executa dentro do processo `worker`, como componente independente do consumer.
Responsabilidade separada não implica processo separado; o
[ADR 0012](decisions/0012-outbox-relay-and-topology.md) registra a decisão e o
caminho de extração para um binário próprio.

- Reservar mensagens vencidas com um lease, em transação curta, para que várias
  instâncias de worker coexistam sem coordenação externa.
- Publicá-las como persistentes no RabbitMQ, sem manter transação aberta durante
  a chamada ao broker.
- Aguardar publisher confirm, e publicar com `mandatory`, antes de marcar a
  publicação como concluída: um confirm sozinho não prova que alguma fila
  recebeu a mensagem.
- Manter um deadline no transporte durante toda a tentativa e encerrar a janela
  de publicação antes do lease, reservando tempo para gravar seu desfecho.
- Reagendar com backoff e jitter as publicações que falharem, sem nunca
  descartar a mensagem por indisponibilidade do broker.
- Abandonar apenas mensagens cujo erro seja classificado como permanente, e
  reportar as que insistirem em falhar sem interromper as tentativas.

### Payments consumer

Executa dentro do processo `worker`, ao lado do relay, com conexões AMQP
próprias. O [ADR 0013](decisions/0013-consumer-transactions-transitions-and-retry.md)
registra a transação, a matriz de transições e a política de falha.

- Consumir com ack manual, prefetch explícito e concorrência limitada.
- Bloquear o agregado inteiro — evento, tentativa, pagamento e pedido — em
  ordem fixa, para que eventos diferentes do mesmo pagamento não se atropelem.
- Ler o estado bloqueado, consultar a matriz de transições e só então escrever;
  uma transição aprovada que não altere exatamente uma linha é invariante
  violada, não no-op.
- Confirmar a mensagem somente após o commit no PostgreSQL.
- Republicar falhas transitórias em faixas de retry com TTL crescente, e falhas
  definitivas na dead-letter exchange, sempre com publisher confirm antes do
  ack.
- Nunca regredir um estado final por causa de um evento fora de ordem.
- Validar provider, valor, moeda e a relação dos identificadores antes de
  aplicar qualquer transição.

### RabbitMQ — uso financeiro

- Manter filas duráveis e mensagens persistentes.
- Entregar mensagens novamente quando um consumer falhar antes do ack.
- Permitir concorrência controlada por prefetch.
- Isolar mensagens que excederem o limite de tentativas.

### Stripe

- Hospedar a interface que coleta o instrumento de pagamento.
- Tokenizar e processar os dados sensíveis.
- Executar autenticações exigidas pelo meio de pagamento.
- Notificar alterações de estado por eventos assinados.

### Integrador do boilerplate

- Consumir as rotas de negócio pelo seu próprio backend; uma credencial da API
  nunca deve ser distribuída em frontend ou aplicativo do consumidor.
- Configurar credenciais e endpoints corretamente.
- Avaliar suas obrigações PCI DSS, LGPD e demais normas aplicáveis.
- Definir retenção, observabilidade, resposta a incidentes e controle de acesso.
- Verificar os requisitos do adquirente e do provedor escolhidos.

## Limites de confiança e acesso

Cada implantação atende um único integrador e possui banco, credenciais Stripe
e chaves de acesso próprios. Não há isolamento multi-tenant dentro da aplicação:
depois de autenticado, o integrador pode operar todos os pedidos da instalação.

```text
backend do integrador -- X-API-Key ------> rotas de pedido e checkout
Stripe --------------- Stripe-Signature -> webhook
infraestrutura -------- rede/probe ------> health
desenvolvedor ---------- ambiente local --> Swagger UI
```

A camada HTTP aplica autenticação por API key às operações de negócio. A
política nega acesso por padrão e libera explicitamente apenas health e o
webhook assinado, cuja confiança vem da verificação da assinatura sobre o corpo
bruto. O domínio e os casos de uso não conhecem headers ou credenciais.

A documentação é opt-in por `DOCS_ENABLED`, desligada por padrão em todos os
ambientes, e exige `X-API-Key` fora de `development`. Toda resposta carrega
headers de segurança, nenhuma emite CORS e `X-Forwarded-For` só é acreditado
quando o peer pertence a `TRUSTED_PROXY_CIDRS`. TLS, gestão de secrets e
controles de borda continuam sob responsabilidade de quem implanta o projeto.
O modelo completo está no
[ADR 0016](decisions/0016-http-surface-and-client-identity.md).

O modelo completo, alternativas e limitações estão no
[ADR 0010](decisions/0010-route-access-model.md).

## Modelo de domínio e schema

`Order`, `Payment`, `PaymentAttempt`, `WebhookEvent` e `OutboxEvent` possuem
entidades Go e persistência. As transições de estado disparadas pelos eventos
recebidos são aplicadas pelo consumer, contra a matriz registrada no
[ADR 0013](decisions/0013-consumer-transactions-transitions-and-retry.md).

### Customer — fora da versão 0.1

Não está modelado. Se entrar em uma evolução posterior, deverá armazenar somente
os dados necessários ao caso de uso.

### Order

Representa a intenção comercial da aplicação. Contém valor, moeda e itens
necessários para explicar a cobrança. O valor enviado ao provedor deve ser
derivado do pedido persistido, nunca de um valor aceito diretamente do cliente.

### Payment

Representa a operação financeira associada a um pedido e seu estado consolidado.

### PaymentAttempt

Registra uma tentativa de criar ou executar a operação no provedor, incluindo a
chave de idempotência e o identificador externo.

### Refund — evolução posterior

Reservado no modelo para evolução posterior. A automação de reembolsos não faz
parte da versão 0.1.

### WebhookEvent

Inbox persistente dos eventos recebidos. Guarda identificador externo, tipo,
datas de recebimento e processamento, estado do processamento e informação de
erro suficiente para reprocessamento seguro.

### OutboxEvent

Registra a intenção de publicar uma mensagem. É criado na mesma transação que o
`WebhookEvent` e só será marcado como publicado após a confirmação do RabbitMQ,
o que o relay faz. Uma publicação poderá se repetir, portanto o
consumidor precisa ser idempotente.

A linha não guarda o payload do provedor. A mensagem publicada carrega apenas
`messageId`, tipo, versão do schema, instante de ocorrência, correlação e a
referência ao `WebhookEvent`; o consumidor relê a inbox para obter a cópia
canônica. O tipo publicado usa vocabulário de domínio, não o nome do evento no
provedor, o que mantém o contrato de mensageria neutro. As razões estão no
[ADR 0011](decisions/0011-webhook-reception-and-outbox.md).

## Estado inicial de pagamento

```text
pending
  |-- processing
  |     |-- succeeded
  |     `-- failed
  `-- cancelled

succeeded -> partially_refunded -> refunded
```

Os estados de reembolso já são aceitos pelo schema para compatibilidade futura,
mas ainda não pertencem à entidade Go nem precisam de casos de uso públicos na
primeira versão.

## Invariantes

As invariantes abaixo são executáveis, incluindo as de consumo.

- Dinheiro é representado por inteiro na menor unidade e código de moeda.
- Valor e moeda tornam-se imutáveis quando o checkout é iniciado.
- Toda escrita remota recebe uma chave de idempotência estável.
- Cada evento externo é persistido antes de produzir efeitos de negócio.
- O evento de inbox e sua mensagem de outbox são criados na mesma transação.
- Uma mensagem só é marcada como publicada após publisher confirm.
- O consumer usa ack manual depois que seus efeitos são persistidos.
- Toda mensagem pode ser entregue mais de uma vez.
- A assinatura é validada sobre o corpo bruto antes da desserialização normal.
- O identificador do evento impede o processamento repetido.
- Uma resposta HTTP de sucesso ao webhook só é enviada depois que o evento foi
  aceito de forma durável; o trabalho demorado ocorre fora da requisição.
- Transições inválidas ou regressivas são ignoradas ou encaminhadas para
  análise: um evento negativo depois de um estado final positivo é no-op, e um
  evento positivo sobre um pagamento ou tentativa final contraditória vai para
  a dead-letter. Estados de reembolso nunca são regredidos por eventos tardios
  de checkout.
- Chamadas do cliente e URLs de retorno não determinam o estado final do
  pagamento.
- Logs usam identificadores e metadados mínimos, nunca secrets ou instrumentos
  completos de pagamento.

## Organização atual

Antes de haver necessidade comprovada de pacotes independentes:

```text
cmd/
  api/
    main.go
  worker/
    main.go
  migrate/
    main.go
internal/
  domain/
    orders/
    payments/
    webhooks/
  application/
    orders/
    outbox/
    payments/
    webhooks/
  adapters/
    catalog/
    postgres/
    rabbitmq/
    payments/
      stripe/
  transport/
    http/
  platform/
    auth/
    config/
    database/
    logging/
    telemetry/
api/
  openapi.yaml
db/
  migrations/
  queries/
docs/
tests/
```

Os repositories de pedido e pagamento usam GORM com transações curtas e apoiam
as garantias de concorrência nas constraints do schema. A inbox, a outbox, o
lease do relay e as transições do consumer usam `sqlc` sobre `database/sql`,
porque a forma dessas queries faz parte da garantia. Nenhuma transação mistura
os dois estilos: o caminho de escrita do consumer é inteiramente `sqlc`, o que
mantém a regra do ADR 0007 sem precisar de uma ponte sobre o mesmo `sql.Tx`.

A fronteira entre os dois estilos é por query, e não por tabela: `payments`,
`payment_attempts` e `orders` são escritas por GORM na API e por `sqlc` no
worker. Goose é a única autoridade de migrations e o projeto não usa
`AutoMigrate`.

## Implantação inicial

O primeiro destino documentado é a Railway. API e worker são serviços
independentes construídos da mesma imagem; somente a API recebe domínio público.
PostgreSQL e RabbitMQ permanecem na rede privada do projeto, e o broker usa
volume persistente. Migrations rodam como etapa anterior ao deploy da API.

O ambiente local pode habilitar Grafana, Prometheus, Tempo, Loki e o Collector
por meio do profile de observabilidade do Docker Compose, que também provisiona
o dashboard versionado do pipeline. Em produção, o destino OTLP é configuração
externa e não faz parte do domínio.

As métricas da aplicação vivem em `internal/platform/metrics`, único pacote fora
de `internal/platform/telemetry` que importa a API de métricas do OpenTelemetry.
Ele implementa as interfaces de observer que aplicação e adapters já declaravam,
de modo que domínio e casos de uso permanecem sem dependência de telemetria.
Toda label passa por uma allowlist explícita, e estado como backlog e
profundidade de fila é amostrado em background, nunca dentro de um callback de
coleta. Ver [ADR 0015](decisions/0015-application-metrics-and-cardinality.md).

Depois de uma implementação real e estável, as fronteiras reutilizáveis podem
ser extraídas:

```text
pkg/core
pkg/<provider>
```

## Interface do provedor

A aplicação já usa uma porta pequena para a operação exigida pela Fase 2:

```text
CreateCheckout(ctx, trusted order data, idempotencyKey) -> checkout reference
```

A verificação de webhook já usa uma segunda porta pequena, implementada pelo
mesmo adapter:

```text
Verify(rawBody, signature) -> provider event com significado de domínio
```

O adapter traduz o nome do evento no provedor para o vocabulário do domínio.
Um evento que não mapeia para nenhum significado conhecido é registrado e
ignorado, nunca rejeitado como se fosse forjado.

Operações não usadas não devem ser adicionadas para tentar antecipar todos os
provedores.

## Garantia de publicação

PostgreSQL e RabbitMQ não compartilham uma transação. Publicar diretamente no
broker depois de salvar o webhook criaria uma janela de perda entre as duas
operações. O transactional outbox fecha essa janela, e as duas etapas abaixo já são
executáveis:

```text
BEGIN
  INSERT webhook_event
  INSERT outbox_event
COMMIT

relay -> publish persistent -> publisher confirm -> mark published
```

Se o relay cair depois do confirm e antes de marcar o registro, a mensagem será
publicada novamente. Por isso a garantia do sistema é entrega pelo menos uma vez
com efeitos idempotentes, e não entrega exatamente uma vez.

## Topologia do RabbitMQ

```text
payments.events (topic, durável)
  |
  +-- payment.webhook.#  -> payments.webhooks (durável)
                              |
                              +-- falha transitória -> retry com backoff
                              `-- rejeição definitiva -> payments.webhooks.dlx
                                                          |
                                                          `-> payments.webhooks.dlq
```

O binding usa `#`, e não `*`. As routing keys carregam o tipo de domínio, como
`payment.webhook.checkout.completed`, e em uma exchange topic o `*` casa
exatamente uma palavra entre pontos: usá-lo descartaria silenciosamente toda
chave com mais de um segmento após o prefixo.

Exchange, filas e mensagens são duráveis, e a topologia é declarada pela
aplicação de forma idempotente na inicialização do worker. A dead-letter é
declarada junto, antes de existir consumer, porque os argumentos de uma fila são
imutáveis no RabbitMQ: adicioná-los depois exigiria apagar e recriar uma fila que
já carrega mensagens financeiras.

O consumer usa ack manual e prefetch explícito. As faixas de retry são objetos
novos, declarados ao lado da topologia existente: uma exchange fanout e uma fila
quorum por faixa, com TTL próprio e dead-lettering de volta à `payments.events`.
Uma fila por faixa evita que uma espera longa na cabeça bloqueie as curtas, e o
fanout preserva a routing key original no caminho de volta.

## Superfície HTTP

Implementada:

```text
POST /v1/orders
POST /v1/orders/{orderId}/checkout
GET  /v1/orders/{orderId}
POST /v1/webhooks/stripe
GET  /health
GET  /docs          (opt-in; autenticado fora de development)
GET  /docs/         (opt-in; autenticado fora de development)
GET  /openapi.yaml  (opt-in; autenticado fora de development)
```

O worker publica somente `GET /health`. As demais operações do contrato não são
registradas naquele processo.

O contrato executável detalhado está em [Contrato da API](api.md).

## Cenários de teste

Cobertos:

- Criação e consulta de pedido com preço definido no servidor.
- Repetição idempotente de pedido e checkout.
- Falha do provedor e resposta de sessão inválida.
- Duas chaves concorrentes disputando o mesmo checkout.
- Tentativa do cliente de informar o próprio valor.
- Assinatura de webhook ausente, forjada ou fora da janela de tempo.
- Corpo de webhook acima do limite do endpoint.
- Mesmo evento entregue duas vezes, sem segunda mensagem de outbox.
- Evento de tipo não tratado, registrado sem produzir mensagem.
- Falha de gravação respondida de forma que o provedor reentregue.

- Cada célula da matriz de transições, incluindo eventos fora de ordem.
- Sessão concluída sem pagamento liquidado, que produz `processing`.
- Versão de API incompatível, metadata ausente e valor ou moeda divergentes.
- Dois eventos diferentes do mesmo pagamento processados ao mesmo tempo.
- Redelivery da mesma mensagem, sem repetir efeitos.
- Falha transitória que passa por uma faixa de retry e volta.
- Falha definitiva que chega à DLQ.
- Falha de processamento ou republicação que fecha o canal e reentrega a
  mensagem sem ack depois do backoff.
- Mensagem malformada republicada explicitamente e confirmada na DLQ.
- Budget esgotado fechando a inbox e enviando a mensagem à DLQ sem divergência
  entre os dois estados.

Planejados para a Fase 4:

- Timeout depois de o provedor aceitar a operação e antes da persistência local.
- RabbitMQ indisponível depois do commit da inbox e do outbox.
- Queda do relay depois do publisher confirm e antes de atualizar o outbox.
- Queda do consumer antes e depois do commit no PostgreSQL.
- Consulta do pedido antes e depois da entrega do webhook.

Os detalhes de criação da sessão, eventos consumidos e testes locais estão no
[plano da integração com Stripe](providers/stripe.md).
