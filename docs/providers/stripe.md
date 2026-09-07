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
- Cartão e Pix como meios de pagamento em BRL, sujeitos à elegibilidade e à
  configuração da conta Stripe.
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

A implementação lê as configurações do ambiente:

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

Para cada `PaymentAttempt`, o adapter cria uma nova Checkout Session. A chamada:

1. usar `mode=payment`, a interface hospedada e oferecer `card` e `pix`;
2. obter valor, moeda e descrição do pedido persistido;
3. enviar um identificador local opaco em `client_reference_id` e em metadata,
   sem incluir dados pessoais;
4. usar uma chave de idempotência estável para a tentativa local;
5. persistir o ID da sessão, sua URL e expiração antes de responder ao cliente.

A reserva local permanece ativa se a chamada ao provedor falhar ou expirar por
timeout. O cliente deve retomar a operação com a mesma `Idempotency-Key`;
enquanto a tentativa estiver ativa, uma chave diferente é bloqueada para evitar
cobranças concorrentes. As transições de falha e expiração são aplicadas pelo
consumer: uma sessão expirada move a tentativa para `expired` e deixa o
pagamento em `pending`, o que libera o índice parcial para uma nova tentativa
dentro do mesmo pagamento.

O SDK oficial está fixado na série major `v86`. A aplicação depende de uma
interface própria e pequena; somente o adapter importa os tipos da Stripe. A
reserva de `Payment` e `PaymentAttempt` ocorre em uma transação local antes da
chamada externa. Se a resposta da Stripe chegar e a gravação local falhar, a
mesma `Idempotency-Key` recupera a sessão remota com segurança na tentativa
seguinte.

## Primeiro checkout em sandbox

1. Copie a chave secreta de teste (`sk_test_...`) do Dashboard para
   `STRIPE_SECRET_KEY` no `.env`.
2. Configure `STRIPE_SUCCESS_URL` e `STRIPE_CANCEL_URL`. Para o teste local, os
   valores da `.env.example` retornam à documentação da API.
3. Inicie PostgreSQL, migrations e API; crie um pedido e abra o checkout com os
   comandos do README.
4. Abra `checkoutUrl` e escolha cartão ou Pix. Para cartão, use
   `4242 4242 4242 4242`, uma data futura e qualquer CVC. Pix depende de uma
   conta Stripe elegível e com o método habilitado.

O pagamento aparece concluído na Stripe, o webhook registra o evento de forma
durável, o relay o publica no RabbitMQ e o consumer aplica a transição: o pedido
chega a `paid`. A URL de sucesso nunca é tratada como prova de pagamento.

### Validação executada

Em 7 de setembro de 2026, o fluxo foi validado de ponta a ponta no sandbox com
o produto de demonstração de R$ 100,00. A Stripe confirmou a Checkout Session
como `complete`, com `payment_status=paid`, `amount_total=10000`, moeda `brl` e
`livemode=false`. Repetir a requisição com a mesma chave devolveu a mesma sessão
e o PostgreSQL permaneceu com um único `Payment` e um único `PaymentAttempt`.

Naquele momento, antes do consumer existir, pedido, pagamento e tentativa
continuaram localmente em `pending`. Com a terceira entrega da Fase 3 o mesmo
evento passa a levar o pedido a `paid`; nenhuma URL de retorno altera estado.

## Webhooks e transições

O endpoint é `POST /v1/webhooks/stripe`. Ele limita o tamanho do corpo,
preserva os bytes originais e verifica o header `Stripe-Signature` com o secret
do endpoint antes de desserializar o evento. A janela de tolerância padrão do
SDK rejeita assinaturas antigas, o que impede o replay de uma entrega
capturada.

A verificação ignora deliberadamente divergências de versão de API. Este
endpoint apenas registra o evento e nada interpreta o grafo de objetos do
provedor; uma diferença de versão não pode impedir que um evento autêntico seja
gravado. A compatibilidade é verificada pelo consumer, quando ele realmente lê a
sessão: uma versão que o adapter não conhece interrompe o processamento e a
mensagem vai para a dead-letter, em vez de ser interpretada às cegas.

Eventos necessários:

| Evento da Stripe | Efeito esperado |
| --- | --- |
| `checkout.session.completed` | Reconciliar a sessão; concluir somente se `payment_status` indicar pagamento recebido, caso contrário manter em processamento |
| `checkout.session.async_payment_succeeded` | Mover uma tentativa válida para `succeeded` |
| `checkout.session.async_payment_failed` | Mover uma tentativa válida para `failed` |
| `checkout.session.expired` | Cancelar uma tentativa ainda não concluída, sem regredir um estado final |

Um sucesso que contradiga uma tentativa já `failed`, `expired` ou `cancelled`
vai para a DLQ. Em particular, a sessão expirada não pode concluir o pagamento
depois que uma nova tentativa foi liberada. Eventos tardios também não retiram
um pagamento de `partially_refunded` ou `refunded`; eventos não positivos de
uma tentativa antiga encerrada são no-op e não interferem na tentativa nova.

O ID do evento da Stripe é a chave externa de deduplicação da inbox. Um evento
verificado é salvo junto com a outbox em uma única transação e o processamento
de negócio ocorre pelo RabbitMQ. Eventos repetidos ou fora de ordem não repetem
efeitos nem regridem estados finais.

Um evento cujo tipo não esteja na tabela acima é registrado como `skipped` e
não gera mensagem. Isso mantém a trilha de auditoria e avisa quando a Stripe
passa a enviar algo novo, sem que o evento seja tratado como inválido.

O fluxo automatizado de Pix cobre os dois eventos: uma sessão completa com
`payment_status=unpaid` move o pagamento para `processing` sem fechar a
tentativa; `checkout.session.async_payment_succeeded` move tentativa e
pagamento para `succeeded` e o pedido para `paid`. O caminho assíncrono é
validado sem presumir que a conclusão do Checkout significa liquidação.

## Testes de aceitação

### Validados

- Criar um pedido e abrir sua Checkout Session hospedada.
- Repetir a criação com a mesma idempotency key sem criar outra sessão.
- Rejeitar assinatura ausente, forjada ou fora da janela de tolerância.
- Aceitar um evento assinado e gravar inbox e outbox na mesma transação.
- Receber duas vezes o mesmo evento sem criar uma segunda mensagem.
- Registrar um evento de tipo não tratado sem enfileirá-lo.
- Concluir um cartão de teste e observar o pedido local chegar a `paid` depois
  do webhook.
- Receber eventos fora de ordem sem regredir um estado final.
- Aplicar o mesmo evento duas vezes sem repetir efeitos.
- Oferecer Pix na Checkout Session e validar `processing` até `paid` quando o
  evento assíncrono de sucesso chega.

### Planejados para a Fase 4

- Aceitar o webhook de forma durável enquanto RabbitMQ estiver indisponível e
  publicá-lo quando o broker voltar.
- Encerrar API, relay ou worker em pontos críticos sem perder o evento.
- Testar no sandbox o caminho de expiração/falha do Pix com dados reproduzíveis.

A Stripe CLI encaminha eventos ao ambiente local:

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
