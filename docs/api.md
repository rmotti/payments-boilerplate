# Contrato inicial da API

Este documento delimita a superfície HTTP do MVP. A especificação OpenAPI será
a fonte executável do contrato quando a implementação começar.

## Convenções

- Prefixo de versão: `/v1`.
- Corpos de requisição e resposta: JSON.
- Valores monetários: inteiros na menor unidade da moeda.
- Moedas: código explícito, inicialmente `BRL`.
- Identificadores públicos: opacos e não sequenciais.
- Escritas repetíveis: header `Idempotency-Key` obrigatório.
- Erros: estrutura consistente com código, mensagem e identificador de
  correlação.

```json
{
  "code": "invalid_request",
  "message": "quantity must be between 1 and 1000",
  "correlationId": "6f1d2c0a4b8e4f6c9d1e2f3a4b5c6d7e"
}
```

`code` é estável e adequado a tratamento programático. `correlationId` repete o
header `X-Correlation-ID` da resposta; o cliente pode enviá-lo na requisição
para correlacionar seus próprios logs com os da API.

O prefixo `/v1` identifica a primeira geração do contrato HTTP. Enquanto o
projeto estiver na série `v0.x`, ele permanece experimental e pode sofrer
mudanças incompatíveis entre versões minor. Consulte a
[política de versionamento](versioning.md).

## Endpoints

### `POST /v1/orders`

Cria um pedido a partir de um produto conhecido pelo servidor. O cliente não
define livremente o valor que será cobrado: `amount` e `currency` são
calculados a partir do catálogo e da quantidade, e qualquer campo de valor
enviado na requisição é ignorado.

Headers:

| Header | Obrigatório | Regra |
| --- | --- | --- |
| `Idempotency-Key` | sim | Chave opaca de 1 a 255 caracteres, escolhida pelo cliente. |

Requisição:

```json
{
  "productId": "product_demo",
  "quantity": 1
}
```

`productId` deve ter de 1 a 128 caracteres e existir no catálogo. `quantity`
deve estar entre 1 e 1000.

Resposta `201 Created`:

```json
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
| `404` | `product_not_found` | `productId` não existe no catálogo. |
| `409` | `idempotency_key_conflict` | A `Idempotency-Key` já foi usada por outro pedido. |
| `413` | `invalid_request` | Corpo maior que o limite aceito pela API. |
| `500` | `internal_error` | Falha inesperada; a causa fica apenas nos logs. |

Hoje, reenviar a mesma chave devolve `409`. A repetição transparente da criação,
devolvendo o pedido original quando a requisição for idêntica, é o próximo item
do roadmap.

Na versão 0.1 o catálogo é fixo e contém apenas `product_demo`, precificado em
R$ 100,00 (`10000` centavos). Gerenciamento de produtos está fora do contrato.

### `POST /v1/orders/{orderId}/checkout`

Cria ou recupera de forma idempotente uma sessão hospedada no provedor.

```json
{
  "checkoutUrl": "https://checkout.stripe.com/c/pay/...",
  "expiresAt": "2026-09-05T18:00:00Z"
}
```

### `GET /v1/orders/{orderId}`

Retorna o estado conhecido pela API. Este é o endpoint que o sistema integrador
usa depois que o consumidor inicia ou conclui o checkout.

```json
{
  "id": "ord_01J...",
  "status": "paid",
  "amount": 10000,
  "currency": "BRL"
}
```

O `status` do pedido usa vocabulario comercial (`pending`, `paid`, `cancelled`,
`expired`) e nao os estados financeiros da cobranca. Uma tentativa recusada nao
altera o pedido, que permanece `pending` ate ser pago, cancelado ou expirado.

### `POST /v1/webhooks/stripe`

Recebe eventos assinados pela Stripe. Não é um endpoint destinado ao sistema
integrador. O header `Stripe-Signature` deve ser verificado sobre o corpo bruto
antes que o evento seja aceito e persistido.

O Swagger documentará esse endpoint, mas não tentará fabricar assinaturas
válidas. Os testes serão feitos com a Stripe CLI e fixtures controladas.

O mapeamento dos eventos está documentado no
[plano da integração com Stripe](providers/stripe.md).

### `GET /health`

Indica se o processo está disponível. A definição de readiness e a exposição de
detalhes das dependências serão decididas com a infraestrutura.

### `GET /docs`

Expõe o Swagger UI gerado a partir do contrato OpenAPI versionado.

## Fora do contrato da versão 0.1

- Clientes e autenticação de consumidores.
- Catálogo público ou gerenciamento de produtos.
- Reembolsos.
- Assinaturas.
- Operações administrativas.
- Relatórios e conciliação.
- Upload ou captura de dados de cartão.
