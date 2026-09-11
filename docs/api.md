# Contrato da API

Este documento delimita a superfície HTTP do MVP. A especificação OpenAPI é a
fonte executável do contrato e gera o strict server usado pela aplicação.

Somente blocos JSON com o marcador `contract` são exemplos contratuais. Cada
marcador informa `operation`, `direction` e `status`, e a suíte valida o bloco
contra esse ponto exato de `api/openapi.yaml`. Blocos sem o marcador são apenas
ilustrativos e não são interpretados pelo verificador.

## Convenções

- Prefixo de versão: `/v1`.
- Corpos de requisição e resposta: JSON.
- Valores monetários: inteiros na menor unidade da moeda.
- Moedas: código explícito, inicialmente `BRL`.
- Identificadores públicos: opacos e não sequenciais.
- Escritas repetíveis: header `Idempotency-Key` obrigatório.
- Erros: estrutura consistente com código, mensagem e identificador de
  correlação.

```json contract operation=createOrder direction=response status=400 name=common-error
{
  "code": "invalid_request",
  "message": "quantity must be between 1 and 1000",
  "correlationId": "6f1d2c0a4b8e4f6c9d1e2f3a4b5c6d7e"
}
```

`code` é estável e adequado a tratamento programático. `correlationId` repete o
header `X-Correlation-ID` da resposta; o cliente pode enviá-lo na requisição
para correlacionar seus próprios logs com os da API. Um valor fornecido precisa
ser um token ASCII opaco de até 128 bytes, começar por caractere alfanumérico,
conter apenas letras, números, ponto, sublinhado, dois-pontos ou hífen e não ter
formato conhecido de secret. Caso contrário, a API o substitui por um novo
identificador e devolve o valor efetivamente utilizado na resposta.

O prefixo `/v1` identifica a primeira geração do contrato HTTP. Enquanto o
projeto estiver na série `v0.x`, ele permanece experimental e pode sofrer
mudanças incompatíveis entre versões minor. Consulte a
[política de versionamento](versioning.md).

## Modelo de acesso

A versão `0.1` assume um integrador por implantação. O backend do integrador,
nunca o navegador do consumidor, chama as rotas de negócio. A política aprovada
está registrada no [ADR 0010](decisions/0010-route-access-model.md):

| Operação | Acesso |
| --- | --- |
| `POST /v1/orders` | Header `X-API-Key` obrigatório |
| `POST /v1/orders/{orderId}/checkout` | Header `X-API-Key` obrigatório |
| `GET /v1/orders/{orderId}` | Header `X-API-Key` obrigatório |
| `GET /v1/webhook-events` | Header `X-API-Key` obrigatório |
| `POST /v1/webhook-events/{webhookEventId}/reprocess` | Header `X-API-Key` obrigatório |
| `POST /v1/webhooks/stripe` | Sem API key; assinatura Stripe obrigatória |
| `GET /health` | Público, com resposta mínima |
| `GET /docs`, `GET /docs/` e `GET /openapi.yaml` | Ausentes sem `DOCS_ENABLED`; públicos em `APP_ENV=development` com o opt-in; `X-API-Key` obrigatória com o opt-in em qualquer outro ambiente |

O runtime aplica a política por `operationId`, negando por padrão operações que
não aparecem na lista pública. Quando a requisição alcança o middleware de
autenticação, chave ausente ou inválida recebe a mesma resposta `401`, com
código `unauthorized`, sem revelar detalhes sobre as credenciais ativas. O
binding OpenAPI pode rejeitar antes, com `400`, headers obrigatórios ausentes ou
um corpo malformado; em nenhum desses casos o caso de uso é executado. As rotas
da documentação ficam fora do strict server e seguem a política do
[ADR 0016](decisions/0016-http-surface-and-client-identity.md), descrita
adiante.

Autenticação não é limite de uso: toda operação, autenticada ou não, também
passa pelo controle de taxa descrito em [Limite de requisições](#limite-de-requisições).

Uma chave válida dará acesso aos pedidos da própria instalação. Multi-tenancy,
login de consumidores e autorização entre organizações permanecem fora do
contrato da versão `0.1`.

## Endpoints

### `POST /v1/orders`

Cria um pedido a partir de um produto conhecido pelo servidor. O cliente não
define livremente o valor que será cobrado: `amount` e `currency` são
calculados a partir do catálogo e da quantidade, e qualquer campo de valor
enviado na requisição é ignorado.

Headers:

| Header | Obrigatório | Regra |
| --- | --- | --- |
| `X-API-Key` | sim | Chave secreta configurada pelo backend integrador. |
| `Idempotency-Key` | sim | Chave opaca de 1 a 255 caracteres, escolhida pelo cliente. |

Requisição:

```json contract operation=createOrder direction=request status=- name=create-order-request
{
  "productId": "product_demo",
  "quantity": 1
}
```

`productId` deve ter de 1 a 128 caracteres e existir no catálogo. `quantity`
deve estar entre 1 e 1000.

Resposta `201 Created`:

```json contract operation=createOrder direction=response status=201 name=create-order-response
{
  "id": "ord_3f2504e04f8911d39a0c0305e82c3301",
  "status": "pending",
  "amount": 10000,
  "currency": "BRL"
}
```

Erros:

| Status | `code` | Quando |
| --- | --- | --- |
| `400` | `invalid_request` | Header ausente, JSON inválido ou campo fora das regras. |
| `401` | `unauthorized` | `X-API-Key` ausente ou inválida. |
| `404` | `product_not_found` | `productId` não existe no catálogo. |
| `409` | `idempotency_key_conflict` | A chave já pertence a uma criação com produto ou quantidade diferentes. |
| `413` | `invalid_request` | Corpo maior que o limite aceito pela API. |
| `500` | `internal_error` | Falha inesperada; a causa fica apenas nos logs. |

Reenviar a mesma chave com o mesmo `productId` e a mesma `quantity` devolve o
pedido original, inclusive se o catálogo tiver sido reprecificado. Usar a chave
com conteúdo diferente devolve `409`.

Na versão 0.1 o catálogo é fixo e contém apenas `product_demo`, precificado em
R$ 100,00 (`10000` centavos). Gerenciamento de produtos está fora do contrato.

### `POST /v1/orders/{orderId}/checkout`

Cria ou recupera de forma idempotente uma sessão hospedada no provedor.

Headers:

| Header | Obrigatório | Regra |
| --- | --- | --- |
| `X-API-Key` | sim | Chave secreta configurada pelo backend integrador. |
| `Idempotency-Key` | sim | Identifica esta tentativa de checkout. |

O caso de uso carrega o pedido persistido e envia à Stripe somente o preço em
BRL conhecido pelo servidor. A Checkout Session usa `mode=payment`, cartão e Pix,
`client_reference_id` e metadata com IDs locais opacos. A resposta só é enviada
depois que o ID da sessão, a URL e sua expiração estão ligados ao
`PaymentAttempt` local.

Resposta `201 Created`:

```json contract operation=createCheckout direction=response status=201 name=create-checkout-response
{
  "checkoutUrl": "https://checkout.stripe.com/c/pay/...",
  "expiresAt": "2026-09-05T18:00:00Z"
}
```

Repetir a mesma chave para o mesmo pedido devolve a sessão persistida sem criar
outra. Uma chave pertencente a outro pedido, outra tentativa ativa ou um pedido
que não está `pending` devolvem `409`. Falhas do provedor devolvem `502` sem
expor detalhes internos.

### `GET /v1/orders/{orderId}`

Retorna o estado conhecido pela API. Este é o endpoint que o sistema integrador
usa depois que o consumidor inicia ou conclui o checkout.

Requer `X-API-Key` e devolve `404 order_not_found` quando o identificador é
malformado ou não corresponde a um pedido desta instalação. A resposta não
distingue esses casos.

```json contract operation=getOrder direction=response status=200 name=get-order-response
{
  "id": "ord_0123456789abcdef0123456789abcdef",
  "status": "pending",
  "amount": 10000,
  "currency": "BRL"
}
```

O `status` do pedido usa vocabulario comercial (`pending`, `paid`, `cancelled`,
`expired`) e nao os estados financeiros da cobranca. Uma tentativa recusada nao
altera o pedido, que permanece `pending` ate ser pago, cancelado ou expirado.
O pedido passa a `paid` quando o worker processar o evento confirmado pela
Stripe. Para Pix, a sessão completa ainda não paga leva o pagamento a
`processing`; somente o evento assíncrono de sucesso leva o pedido a `paid`.

### `GET /v1/webhook-events`

Lista, com `X-API-Key`, os metadados operacionais mais recentes da inbox e da
outbox. Aceita `status` (`pending`, `processing`, `processed`, `failed` ou
`skipped`) e `limit` de 1 a 100, com padrão 50. Payload bruto e JSON do provedor
nunca aparecem nessa resposta.

### `POST /v1/webhook-events/{webhookEventId}/reprocess`

Reenfileira apenas um evento cuja inbox ou publicação da outbox esteja em
`failed`. A transação bloqueia as duas linhas, reutiliza o `messageId` original,
zera o budget da etapa que falhou e registra `replayCount` e `lastReplayedAt`.
Um replay concorrente ou de trabalho que já voltou a `pending` recebe `409`
`webhook_event_not_replayable`; um ID inexistente recebe `404`
`webhook_event_not_found`.

### `POST /v1/webhooks/stripe`

Recebe eventos assinados pela Stripe. Não é um endpoint destinado ao sistema
integrador e não usa a chave de integração: a confiança vem da verificação
criptográfica do header `Stripe-Signature` sobre os bytes exatos do corpo,
antes de qualquer desserialização.

A operação apenas registra o evento de forma durável e cria, na mesma
transação, a mensagem de outbox correspondente. Nenhum estado de pagamento muda
dentro da requisição; isso pertence ao worker.

A Stripe encerra as tentativas apenas diante de um `2xx` e reenvia o evento
diante de qualquer outra resposta, `400` incluído. Por isso o `2xx` só aparece
quando o evento está gravado; os códigos de erro se distinguem entre si para
quem opera a instalação, não para mudar o que a Stripe faz:

| Situação | Status |
| --- | --- |
| Evento novo aceito e enfileirado | `202` |
| Evento já recebido antes | `200` |
| Tipo de evento não tratado, apenas registrado | `200` |
| Assinatura ausente, inválida ou fora da janela de tempo | `400` |
| Corpo acima do limite do endpoint | `500` |
| Falha ao registrar o evento | `500` |

O `400` de assinatura usa o código estável `invalid_signature` e não distingue
uma assinatura ausente de uma forjada ou expirada, para não revelar a um
atacante qual das três ocorreu. O corpo acima do limite responde `500`, e não
`400`, porque a causa é o limite configurado localmente: é algo que a
instalação conserta e que a entrega seguinte resolve, ao contrário de uma
assinatura que nunca vai verificar. A Stripe reentrega nos dois casos; a
distinção existe para que um limite mal dimensionado não se esconda no ruído
das assinaturas inválidas.

O corpo da resposta segue a estrutura de erro comum, mas a Stripe lê apenas o
status; ele existe para a observabilidade da própria instalação.

O Swagger documenta o endpoint, mas não fabrica assinaturas válidas. Os testes
usam fixtures assinadas localmente e a Stripe CLI.

As decisões de resposta e do conteúdo da mensagem estão no
[ADR 0011](decisions/0011-webhook-reception-and-outbox.md), e o mapeamento dos
eventos no [plano da integração com Stripe](providers/stripe.md).

### `GET /health`

Indica se o processo e suas dependências obrigatórias estão disponíveis. Na API,
o resultado inclui PostgreSQL; no worker, inclui PostgreSQL e RabbitMQ. Retorna
`200` quando todos os checks estão `up` e `503` quando algum está `down`.

### `GET /docs`, `GET /docs/` e `GET /openapi.yaml`

Servem o Swagger UI e o contrato OpenAPI versionado. As três rotas seguem uma
política única, decidida por `DOCS_ENABLED` e `APP_ENV`:

| `APP_ENV` | `DOCS_ENABLED` | Resposta |
| --- | --- | --- |
| qualquer um | `false` (default) | `404`, indistinguível de rota inexistente |
| `development` | `true` | documentação servida sem credencial |
| qualquer outro | `true` | `X-API-Key` válida obrigatória; sem ela, `401` |

A autenticação é decidida antes do redirect de `/docs` para `/docs/` e antes da
verificação de método, de modo que nenhuma resposta revela que as rotas existem.
Habilitar a documentação fora de `development` registra um warning no startup,
que nomeia o ambiente e nunca a chave.

## Limite de requisições

Toda operação é limitada por taxa. A política completa, com o algoritmo e suas
limitações, está no [ADR 0018](decisions/0018-rate-limiting.md); o que segue é
o que um integrador precisa saber para consumir a API.

Cada limite é um **burst**, quantas requisições passam em sequência, e o
**intervalo** em que esse burst se recompõe por inteiro. A taxa sustentada é
burst por intervalo, e o crédito volta continuamente, um token de cada vez, e
não de uma só vez no fim da janela.

| Limite | Contado por | Default | Aplica-se a |
| --- | --- | --- | --- |
| Grosseiro | Endereço do cliente | 1 200 / min | Negócio, operações, documentação e caminhos desconhecidos, antes de parsing e autenticação |
| Por credencial | Credencial válida | 600 / min | Rotas de negócio e operacionais |
| Webhook | Balde global único | 600 / min | `POST /v1/webhooks/stripe`, antes da leitura do corpo |
| Health | Endereço do cliente | 120 / min | `GET /health`, em balde separado |

As rotas de negócio pagam o limite grosseiro e o por credencial. O primeiro é
mais largo por padrão, para limitar a origem sem esconder a quota efetiva da
credencial. Webhook e health não pagam o grosseiro: usam seus baldes dedicados
na mesma posição externa da cadeia. Assim, o endereço da Stripe nunca decide se
uma entrega é aceita, e tráfego comum não derruba a probe.

Uma requisição recusada recebe `429` com o envelope de erro comum e o código
estável `rate_limited`, mais o header `Retry-After` em segundos inteiros:

```json contract operation=createOrder direction=response status=429 name=rate-limited
{
  "code": "rate_limited",
  "message": "too many requests",
  "correlationId": "6f1d2c0a4b8e4f6c9d1e2f3a4b5c6d7e"
}
```

A resposta não informa qual dos limites foi atingido: dizê-lo confirmaria, a
quem tentou adivinhar uma chave, que ela é válida. Uma requisição recusada não
consome crédito, então repetir em laço não afasta o próximo horário permitido —
mas também não o antecipa. Respeite o `Retry-After`.

Uma credencial inválida nunca cria um balde próprio: essas requisições contam
apenas no limite grosseiro e continuam recebendo `401`.

Os valores são configuráveis por ambiente (`RATE_LIMIT_*`, documentadas no
README e em `.env.example`) e podem ser desligados por completo com
`RATE_LIMIT_ENABLED=false`, para quem já limita na borda. Os limites são **por
processo**: uma implantação com várias réplicas admite até N vezes os valores
configurados.

### Headers de resposta

Toda resposta, inclusive `404`, `429` e as geradas pelo recovery, carrega
`X-Correlation-ID`, `X-Content-Type-Options: nosniff`,
`Referrer-Policy: no-referrer`, `Cache-Control: no-store`,
`X-Frame-Options: DENY` e uma `Content-Security-Policy`. Respostas JSON e o
documento OpenAPI usam `default-src 'none'; frame-ancestors 'none'`; a página
do Swagger recebe uma política própria, com os scripts inline liberados por
hash. Uma resposta `429` acrescenta `Retry-After`. A API não emite headers de
CORS.

O worker publica somente `GET /health`. As demais operações do contrato não são
registradas naquele processo e respondem `404`.

## Fora do contrato da versão 0.1

- Clientes e autenticação de consumidores.
- Multi-tenancy e compartilhamento da mesma instalação entre integradores.
- Catálogo público ou gerenciamento de produtos.
- Reembolsos.
- Assinaturas.
- Painel administrativo e operações financeiras manuais.
- Relatórios e conciliação.
- Upload ou captura de dados de cartão.
