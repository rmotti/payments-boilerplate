# Sandbox reproduzível

Este guia reproduz o primeiro pagamento real no modo de teste da Stripe e os
estados assíncronos do pipeline local. O comando `sandbox` usa a API pública
para criar pedidos e receber webhooks; ele consulta o PostgreSQL apenas para
correlacionar a Checkout Session, observar os estados e limpar um pedido local
explicitamente identificado.

O fluxo tem duas fontes de eventos diferentes:

- **Stripe real em modo de teste:** a Stripe CLI encaminha para a API o webhook
  produzido quando uma pessoa conclui a página hospedada;
- **fixtures locais assinadas:** arquivos versionados reproduzem cartão pago,
  Pix em processamento, sucesso/falha assíncrona e sessão expirada sem depender
  do comportamento ou da elegibilidade Pix de uma conta.

Fixtures locais provam o pipeline da aplicação, mas não alegam que a Stripe
produziu aquele evento. Ao investigar uma mudança do provedor, valide também um
Checkout real.

## Proteções

O comando recusa execução quando qualquer uma destas condições não é atendida:

- `APP_ENV` é `development` ou `test`;
- `SANDBOX_API_URL` usa HTTP e aponta para localhost ou loopback;
- `DATABASE_URL` aponta para PostgreSQL em localhost ou loopback nas operações
  que consultam dados;
- a chave Stripe começa com `sk_test_` quando um Checkout real será criado;
- o secret de webhook começa com `whsec_` ao emitir uma fixture.

Pedidos recebem uma chave de idempotência com prefixo `sandbox_order_`. Emissão
de fixtures e limpeza recusam pedidos sem esse marcador. Nenhum comando imprime
API keys, chave Stripe, secret de webhook ou DSN.

## Preparação

São necessários Go, Docker com Compose, uma conta Stripe em modo de teste e a
Stripe CLI autenticada.

Crie a configuração local:

```bash
cp .env.example .env
openssl rand -hex 32
```

Copie a saída para `INTEGRATION_API_KEYS` e uma chave `sk_test_...` para
`STRIPE_SECRET_KEY`. Prepare banco, broker e migrations:

```bash
make sandbox-up
```

Em um terminal, inicie o encaminhamento de webhooks:

```bash
make sandbox-listen
```

Na primeira linha de saída, a Stripe CLI apresenta um signing secret
`whsec_...`. Copie-o para `STRIPE_WEBHOOK_SECRET` no `.env` antes de iniciar a
API. Mantenha o listener aberto.

Em outros dois terminais, inicie os processos:

```bash
make run-api
make run-worker
```

Confirme que configuração, API, banco e Stripe CLI estão prontos:

```bash
make sandbox-doctor
```

O resultado não contém credenciais:

```json
{
  "api": "ready",
  "database": "ready",
  "environment": "development",
  "stripeCLI": "available",
  "stripeMode": "test",
  "webhookSecret": "configured"
}
```

## Checkout real de cartão

Crie um pedido de R$ 100,00 e uma Checkout Session hospedada:

```bash
make sandbox-checkout
```

O comando devolve JSON com `orderId`, valor, expiração e `checkoutUrl`. Abra a
URL e pague com o cartão de teste `4242 4242 4242 4242`, uma data futura e
qualquer CVC. Depois, aguarde o webhook real atravessar inbox, outbox e consumer:

```bash
make sandbox-wait ORDER_ID=ord_...
make sandbox-status ORDER_ID=ord_...
```

O estado final esperado é `order=paid`, `payment=succeeded` e
`attempt=succeeded`. `sandbox-wait` consulta o estado comercial pela API;
`sandbox-status` mostra também pagamento e tentativa persistidos.

## Cenários determinísticos

Cada caminho terminal deve começar com um novo `make sandbox-checkout`. Um
pagamento já liquidado ou uma tentativa encerrada não pode ser reutilizado para
produzir outro resultado.

Os nomes aceitos são:

| Cenário | Evento | Resultado esperado |
| --- | --- | --- |
| `card-paid` | `checkout.session.completed`, pago | pedido pago; pagamento e tentativa com sucesso |
| `pix-processing` | `checkout.session.completed`, ainda não pago | pedido pendente; pagamento processando; tentativa aberta |
| `pix-succeeded` | `checkout.session.async_payment_succeeded` | pedido pago; pagamento e tentativa com sucesso |
| `pix-failed` | `checkout.session.async_payment_failed` | pedido pendente; pagamento e tentativa com falha |
| `checkout-expired` | `checkout.session.expired` | pedido e pagamento pendentes; tentativa expirada |

Exemplo completo do Pix:

```bash
make sandbox-checkout
make sandbox-event ORDER_ID=ord_... SCENARIO=pix-processing
make sandbox-event ORDER_ID=ord_... SCENARIO=pix-succeeded
```

O próprio comando aguarda até 20 segundos pelo estado esperado e devolve os
estados de order, payment, attempt, inbox e outbox. Use `WAIT=0s` apenas quando
quiser enviar o evento sem aguardar o worker.

Falha e expiração usam pedidos independentes:

```bash
make sandbox-event ORDER_ID=ord_de_um_checkout SCENARIO=pix-failed
make sandbox-event ORDER_ID=ord_de_outro_checkout SCENARIO=checkout-expired
```

Para provar deduplicação, fixe o mesmo ID nas duas entregas:

```bash
make sandbox-event ORDER_ID=ord_... SCENARIO=card-paid EVENT_ID=evt_sandbox_duplicate
make sandbox-event ORDER_ID=ord_... SCENARIO=card-paid EVENT_ID=evt_sandbox_duplicate
```

A segunda requisição encontra a inbox original e não cria outra outbox nem
repete o efeito financeiro.

## RabbitMQ indisponível

Pare apenas o broker, emita a fixture sem aguardar e confirme que a inbox/outbox
permanece durável:

```bash
docker compose stop rabbitmq
make sandbox-event ORDER_ID=ord_... SCENARIO=card-paid WAIT=0s
make sandbox-status ORDER_ID=ord_... EVENT_ID=evt_sandbox_...
docker compose start rabbitmq
make sandbox-status ORDER_ID=ord_... EVENT_ID=evt_sandbox_...
```

Depois da reconexão, o relay publica o backlog e o consumer leva o agregado ao
estado terminal. O `providerEventId` é exibido por `sandbox-event` mesmo quando
a espera está desligada.

## Limpeza local

Encerre qualquer cenário ainda em andamento antes de limpar. O comando recusa
inbox em `pending`/`processing` e outbox ainda não liquidada. Ele remove somente
as linhas locais relacionadas ao `ORDER_ID` explícito e somente se o pedido tem
o marcador de idempotência criado pelo sandbox:

```bash
make sandbox-clean ORDER_ID=ord_...
```

A Checkout Session remota de teste não é apagada; ela expira na Stripe. Volumes
PostgreSQL e RabbitMQ também não são removidos por esse comando.

## Testes automatizados equivalentes

O caminho determinístico completo também faz parte da suíte E2E e usa a mesma
composição da API e do worker:

```bash
make test-e2e
```

Essa suíte injeta um provider local no lugar da chamada externa. O Checkout
manual acima continua sendo a prova do adapter Stripe e da conta de sandbox.
