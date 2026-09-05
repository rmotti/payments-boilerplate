# Arquitetura inicial

Este documento descreve as fronteiras e invariantes da API em Go. A aplicação
usa PostgreSQL como fonte de verdade e RabbitMQ para processamento assíncrono.

## Contexto

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
A API cria e consulta suas próprias entidades de negócio e recebe o resultado
assíncrono por um endpoint de webhook. O Swagger UI documenta e exercita o
contrato, mas não faz parte do fluxo de produção de quem adotar o projeto.

A API persiste o webhook e uma mensagem de outbox na mesma transação. O relay
publica a mensagem no RabbitMQ e o worker aplica seus efeitos de forma
idempotente. API, relay e consumer pertencem ao mesmo código-base.

## Responsabilidades

### Payments API

- Criar o pedido e calcular seu valor no servidor.
- Gerar e persistir uma chave de idempotência.
- Solicitar a criação do checkout ao provedor.
- Associar identificadores locais aos identificadores externos.
- Validar e armazenar eventos recebidos.
- Criar a mensagem de outbox na mesma transação do evento.
- Expor ao sistema integrador o estado conhecido pela API.
- Publicar um contrato OpenAPI coerente com a implementação.

### Outbox relay

- Buscar mensagens de outbox ainda não publicadas.
- Publicá-las como persistentes no RabbitMQ.
- Aguardar publisher confirm antes de marcar a publicação como concluída.
- Repetir publicações que falharem sem perder a mensagem original.

### Payments worker

- Consumir mensagens com confirmação manual.
- Aplicar transições de estado válidas e idempotentes.
- Confirmar a mensagem somente após o commit no PostgreSQL.
- Aplicar retry com backoff para falhas transitórias.
- Encaminhar falhas definitivas para uma dead-letter queue.

### RabbitMQ

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

- Consumir a API por seu próprio backend, frontend ou aplicativo.
- Configurar credenciais e endpoints corretamente.
- Avaliar suas obrigações PCI DSS, LGPD e demais normas aplicáveis.
- Definir retenção, observabilidade, resposta a incidentes e controle de acesso.
- Verificar os requisitos do adquirente e do provedor escolhidos.

## Modelo de domínio mínimo

### Customer

Referência local para a pessoa ou organização compradora. Deve armazenar somente
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

### Refund

Reservado no modelo para evolução posterior. A automação de reembolsos não faz
parte da versão 0.1.

### WebhookEvent

Inbox persistente dos eventos recebidos. Guarda identificador externo, tipo,
datas de recebimento e processamento, estado do processamento e informação de
erro suficiente para reprocessamento seguro.

### OutboxEvent

Registra a intenção de publicar uma mensagem. É criado na mesma transação que o
`WebhookEvent` e só é marcado como publicado após a confirmação do RabbitMQ.
Uma publicação pode se repetir, portanto o consumidor continua idempotente.

## Estado inicial de pagamento

```text
pending
  |-- processing
  |     |-- succeeded
  |     `-- failed
  `-- cancelled

succeeded -> partially_refunded -> refunded
```

Os estados de reembolso estão previstos para compatibilidade futura, mas não
precisam de casos de uso públicos na primeira versão.

## Invariantes

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
- Transições inválidas ou regressivas são ignoradas ou encaminhadas para análise.
- Chamadas do cliente e URLs de retorno não determinam o estado final do
  pagamento.
- Logs usam identificadores e metadados mínimos, nunca secrets ou instrumentos
  completos de pagamento.

## Organização proposta

Antes de haver necessidade comprovada de pacotes independentes:

```text
cmd/
  api/
    main.go
  worker/
    main.go
internal/
  domain/
    orders/
    payments/
  application/
  adapters/
    postgres/
    rabbitmq/
    payments/
      stripe/
  transport/
    http/
api/
  openapi.yaml
db/
  migrations/
  queries/
docs/
tests/
```

Depois de uma implementação real e estável, as fronteiras reutilizáveis podem
ser extraídas:

```text
pkg/core
pkg/<provider>
```

## Interface do provedor

A interface inicial deve conter somente operações exigidas pelo fluxo real. Uma
forma conceitual, ainda não normativa, seria:

```text
CreateCheckout(ctx, order, idempotencyKey) -> checkout reference
ParseAndVerifyWebhook(rawBody, headers) -> provider event
MapEvent(providerEvent) -> domain event
```

Operações não usadas não devem ser adicionadas para tentar antecipar todos os
provedores.

## Garantia de publicação

PostgreSQL e RabbitMQ não compartilham uma transação. Publicar diretamente no
broker depois de salvar o webhook criaria uma janela de perda entre as duas
operações. O transactional outbox fecha essa janela:

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

## Topologia inicial do RabbitMQ

```text
payments.events exchange
  |
  +-- payment.webhook.* -> payments.webhooks queue
                              |
                              +-- falha transitória -> retry com backoff
                              `-- tentativas esgotadas -> payments.webhooks.dlq
```

Exchange, filas e mensagens devem ser duráveis. O consumer usa ack manual e um
prefetch baixo e configurável. A topologia será declarada pela aplicação de
forma idempotente.

## Superfície HTTP do MVP

```text
POST /v1/orders
POST /v1/orders/{orderId}/checkout
GET  /v1/orders/{orderId}
POST /v1/webhooks/stripe
GET  /health
GET  /docs
```

O contrato detalhado está em [Contrato inicial da API](api.md). Endpoints de
webhook aparecem na especificação para documentação, mas seus testes reais
exigem uma assinatura válida gerada pelo provedor ou por sua ferramenta local.

## Cenários mínimos de teste

- Criação normal de pedido e checkout.
- Repetição da criação com a mesma chave.
- Timeout depois de o provedor aceitar a operação.
- Assinatura de webhook inválida.
- Mesmo evento entregue duas vezes.
- Eventos relacionados entregues fora de ordem.
- Falha no processamento depois de o evento ser persistido.
- RabbitMQ indisponível depois do commit da inbox e do outbox.
- Queda do relay depois do publisher confirm e antes de atualizar o outbox.
- Queda do consumer antes e depois do commit no PostgreSQL.
- Redelivery da mesma mensagem.
- Mensagem que excede o limite de tentativas e chega à DLQ.
- Tentativa de alterar valor ou moeda após iniciar o checkout.
- Consulta do pedido antes e depois da entrega do webhook.

Os detalhes de criação da sessão, eventos consumidos e testes locais estão no
[plano da integração com Stripe](providers/stripe.md).
