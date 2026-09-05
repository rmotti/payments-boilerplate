# Integração com Stripe

Este documento define a primeira integração concreta do Payments Boilerplate.
O objetivo é entregar uma fatia vertical pequena, segura e compreensível antes
de tentar generalizar o projeto para outros provedores.

## Por que Stripe

- Oferece Checkout hospedado, reduzindo a superfície que entra em contato com
  dados de cartão.
- Possui SDK oficial para Go, sandbox, cartões de teste e CLI para encaminhar
  webhooks ao ambiente local.
- Checkout Sessions cobre o pagamento único necessário ao MVP.
- Está disponível no Brasil e oferece Pix para contas elegíveis.

A escolha é do primeiro adapter, não uma promessa de que Stripe será sempre o
melhor provedor para todo negócio. Taxas, elegibilidade, métodos habilitados e
requisitos da conta devem ser avaliados por cada pessoa que adotar o projeto.

## Escopo da versão 0.1

- Stripe Checkout em página hospedada.
- Checkout Session com `mode=payment`.
- Uma moeda: `BRL`.
- Cartão como primeiro meio de pagamento.
- Pix depois que inbox, outbox, RabbitMQ e worker estiverem validados com
  cartão.
- SDK oficial `stripe-go`.
- Versão do SDK fixada e atualizada deliberadamente, com testes de contrato.
- Ambiente de testes da Stripe e Stripe CLI para webhooks locais.

Não fazem parte deste adapter na versão `0.1`:

- Stripe Billing e assinaturas.
- Stripe Connect, marketplace, split e payouts.
- Stripe Elements, Payment Element ou checkout embutido/customizado.
- Armazenamento de cartões, gestão de Customer ou cobrança off-session.
- Stripe Tax, invoicing, disputas e reembolsos automatizados.

## Configuração

A implementação deverá ler as configurações do ambiente:

| Variável | Finalidade |
| --- | --- |
| `STRIPE_SECRET_KEY` | Autenticar chamadas do backend à API da Stripe |
| `STRIPE_WEBHOOK_SECRET` | Verificar assinaturas do endpoint de webhook |
| `STRIPE_SUCCESS_URL` | Retorno do consumidor após o Checkout |
| `STRIPE_CANCEL_URL` | Retorno quando o consumidor cancela o Checkout |

As URLs de retorno melhoram a experiência do consumidor, mas não confirmam o
resultado do pagamento. O estado local só muda por informação verificada do
provedor.

## Criação do Checkout

Para cada `PaymentAttempt`, o adapter cria uma nova Checkout Session. A chamada
deve:

1. usar `mode=payment` e a interface hospedada;
2. obter valor, moeda e descrição do pedido persistido;
3. enviar um identificador local opaco em `client_reference_id` e em metadata,
   sem incluir dados pessoais;
4. usar uma chave de idempotência estável para a tentativa local;
5. persistir o ID da sessão, sua URL e expiração antes de responder ao cliente.

A mesma chave é reutilizada ao repetir a mesma tentativa após timeout. Uma nova
tentativa de pagamento recebe uma nova chave e uma nova Checkout Session.

## Webhooks e transições

O endpoint inicial será `POST /v1/webhooks/stripe`. A implementação deve limitar
o tamanho do corpo, preservar seus bytes originais e verificar o header
`Stripe-Signature` com o secret do endpoint antes de desserializar o evento.

Eventos necessários:

| Evento da Stripe | Efeito esperado |
| --- | --- |
| `checkout.session.completed` | Reconciliar a sessão; concluir somente se `payment_status` indicar pagamento recebido, caso contrário manter em processamento |
| `checkout.session.async_payment_succeeded` | Mover uma tentativa válida para `succeeded` |
| `checkout.session.async_payment_failed` | Mover uma tentativa válida para `failed` |
| `checkout.session.expired` | Cancelar uma tentativa ainda não concluída, sem regredir um estado final |

O ID do evento da Stripe é a chave externa de deduplicação da inbox. Um evento
verificado é salvo junto com a outbox em uma única transação; o processamento de
negócio ocorre pelo RabbitMQ. Eventos repetidos ou fora de ordem não podem
repetir efeitos nem regredir estados finais.

Mesmo que cartão costume ter confirmação imediata, o código não deve presumir
que todo meio de pagamento conclui durante a requisição. Esse limite prepara o
fluxo para Pix e outros métodos assíncronos.

## Testes de aceitação

- Criar um pedido e abrir sua Checkout Session hospedada.
- Concluir um cartão de teste e observar o pedido chegar a `succeeded`.
- Repetir a criação com a mesma idempotency key sem criar outra sessão.
- Rejeitar assinatura ausente ou inválida.
- Receber duas vezes o mesmo evento sem duplicar efeitos.
- Receber eventos fora de ordem sem regredir um estado final.
- Aceitar o webhook de forma durável enquanto RabbitMQ estiver indisponível e
  publicá-lo quando o broker voltar.
- Encerrar API, relay ou worker em pontos críticos sem perder o evento.
- Depois do fluxo de cartão estar estável, testar Pix de `processing` até o
  estado final e o caminho de expiração/falha.

Para desenvolvimento local, a Stripe CLI poderá encaminhar eventos ao endpoint:

```shell
stripe listen --forward-to localhost:8080/v1/webhooks/stripe
```

## Referências oficiais

- [Stripe Checkout](https://docs.stripe.com/payments/checkout)
- [Criar uma Checkout Session](https://docs.stripe.com/api/checkout/sessions/create)
- [Fulfillment e eventos assíncronos](https://docs.stripe.com/checkout/fulfillment)
- [Webhooks com Go](https://docs.stripe.com/webhooks?lang=go)
- [Pix](https://docs.stripe.com/payments/pix)
- [Stripe no Brasil](https://stripe.com/global)
