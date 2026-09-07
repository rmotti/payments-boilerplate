# Arquitetura

Este documento descreve as fronteiras e invariantes da API em Go e distingue o
runtime atual da arquitetura-alvo da versão `0.1.0`. Ao fim da Fase 2, a API usa
PostgreSQL como fonte de verdade e abre Stripe Checkout; o processo `worker`
apenas verifica PostgreSQL e RabbitMQ e expõe health. Webhook, inbox/outbox,
relay e consumo assíncrono pertencem à Fase 3.

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
A API já cria e consulta suas próprias entidades de negócio. Na Fase 3, ela
receberá o resultado assíncrono por um endpoint de webhook. O Swagger UI
documenta e exercita o contrato, mas não faz parte do fluxo de produção de quem
adotar o projeto.

Na arquitetura-alvo, a API persistirá o webhook e uma mensagem de outbox na
mesma transação. O relay publicará a mensagem no RabbitMQ e o worker aplicará
seus efeitos de forma idempotente. API, relay e consumer pertencerão ao mesmo
código-base.

## Responsabilidades

### Payments API

- Criar o pedido e calcular seu valor no servidor.
- Receber, validar e persistir a chave de idempotência escolhida pelo integrador.
- Solicitar a criação do checkout ao provedor.
- Associar identificadores locais aos identificadores externos.
- Na Fase 3, validar e armazenar eventos recebidos.
- Na Fase 3, criar a mensagem de outbox na mesma transação do evento.
- Expor ao sistema integrador o estado conhecido pela API.
- Publicar um contrato OpenAPI coerente com a implementação.

### Outbox relay — Fase 3

- Buscar mensagens de outbox ainda não publicadas.
- Publicá-las como persistentes no RabbitMQ.
- Aguardar publisher confirm antes de marcar a publicação como concluída.
- Repetir publicações que falharem sem perder a mensagem original.

### Payments worker — Fase 3

O binário atual conecta as dependências e serve health, mas ainda não consome
mensagens. Sua responsabilidade-alvo é:

- Consumir mensagens com confirmação manual.
- Aplicar transições de estado válidas e idempotentes.
- Confirmar a mensagem somente após o commit no PostgreSQL.
- Aplicar retry com backoff para falhas transitórias.
- Encaminhar falhas definitivas para uma dead-letter queue.

### RabbitMQ — uso financeiro na Fase 3

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
Stripe --------------- Stripe-Signature -> webhook (Fase 3)
infraestrutura -------- rede/probe ------> health
desenvolvedor ---------- ambiente local --> Swagger UI
```

A camada HTTP aplica autenticação por API key às operações de negócio. A
política nega acesso por padrão e libera explicitamente apenas health. O
webhook assinado será uma exceção pública quando entrar na Fase 3. O domínio e
os casos de uso não conhecem headers ou credenciais.

O comando `api` atual serve Swagger UI e o documento OpenAPI publicamente em
qualquer `APP_ENV`. Desabilitar ou proteger essa documentação fora do ambiente
de desenvolvimento permanece na Fase 4. TLS, gestão de secrets e controles de
borda continuam sob responsabilidade de quem implanta o projeto.

O modelo completo, alternativas e limitações estão no
[ADR 0010](decisions/0010-route-access-model.md).

## Modelo de domínio e schema

`Order`, `Payment` e `PaymentAttempt` já possuem entidades Go e persistência.
`WebhookEvent` possui schema preparado, mas seu caso de uso entra na Fase 3.

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

### WebhookEvent — schema pronto, uso na Fase 3

Inbox persistente dos eventos recebidos. Guarda identificador externo, tipo,
datas de recebimento e processamento, estado do processamento e informação de
erro suficiente para reprocessamento seguro.

### OutboxEvent — Fase 3

Ainda não está modelado. Registrará a intenção de publicar uma mensagem, será
criado na mesma transação que o `WebhookEvent` e só será marcado como publicado
após a confirmação do RabbitMQ. Uma publicação poderá se repetir, portanto o
consumidor continuará idempotente.

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

As invariantes de pedido, preço, checkout e idempotência já são executáveis. As
que mencionam webhook, outbox, publicação ou consumo descrevem a Fase 3.

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
  application/
    orders/
    payments/
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

Os repositories atuais usam GORM com transações curtas e apoiam as garantias de
concorrência nas constraints do schema. A geração com `sqlc` está preparada
para o SQL crítico da Fase 3, como locks, polling e transições condicionais.
Goose é a única autoridade de migrations e o projeto não usa `AutoMigrate`.

## Implantação inicial

O primeiro destino documentado é a Railway. API e worker são serviços
independentes construídos da mesma imagem; somente a API recebe domínio público.
PostgreSQL e RabbitMQ permanecem na rede privada do projeto, e o broker usa
volume persistente. Migrations rodam como etapa anterior ao deploy da API.

O ambiente local pode habilitar Grafana, Prometheus, Tempo, Loki e o Collector
por meio do profile de observabilidade do Docker Compose. Em produção, o destino
OTLP é configuração externa e não faz parte do domínio.

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

Na Fase 3, as operações de webhook entram apenas quando seus casos de uso forem
implementados:

```text
ParseAndVerifyWebhook(rawBody, headers) -> provider event
MapEvent(providerEvent) -> domain event
```

Operações não usadas não devem ser adicionadas para tentar antecipar todos os
provedores.

## Garantia de publicação — Fase 3

PostgreSQL e RabbitMQ não compartilham uma transação. Publicar diretamente no
broker depois de salvar o webhook criaria uma janela de perda entre as duas
operações. Na Fase 3, o transactional outbox fechará essa janela:

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

## Topologia inicial do RabbitMQ — Fase 3

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

## Superfície HTTP

Implementada ao fim da Fase 2:

```text
POST /v1/orders
POST /v1/orders/{orderId}/checkout
GET  /v1/orders/{orderId}
GET  /health
GET  /docs
GET  /openapi.yaml
```

Planejada para a Fase 3:

```text
POST /v1/webhooks/stripe
```

O contrato executável detalhado está em [Contrato da API](api.md). O endpoint de
webhook só entrará na especificação quando puder validar uma assinatura real e
persistir o evento de forma durável.

## Cenários de teste

Cobertos na Fase 2:

- Criação e consulta de pedido com preço definido no servidor.
- Repetição idempotente de pedido e checkout.
- Falha do provedor e resposta de sessão inválida.
- Duas chaves concorrentes disputando o mesmo checkout.
- Tentativa do cliente de informar o próprio valor.

Planejados para as Fases 3 e 4:

- Timeout depois de o provedor aceitar a operação e antes da persistência local.
- Assinatura de webhook inválida.
- Mesmo evento entregue duas vezes.
- Eventos relacionados entregues fora de ordem.
- Falha no processamento depois de o evento ser persistido.
- RabbitMQ indisponível depois do commit da inbox e do outbox.
- Queda do relay depois do publisher confirm e antes de atualizar o outbox.
- Queda do consumer antes e depois do commit no PostgreSQL.
- Redelivery da mesma mensagem.
- Mensagem que excede o limite de tentativas e chega à DLQ.
- Consulta do pedido antes e depois da entrega do webhook.

Os detalhes de criação da sessão, eventos consumidos e testes locais estão no
[plano da integração com Stripe](providers/stripe.md).
