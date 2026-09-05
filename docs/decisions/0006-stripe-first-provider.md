# ADR 0006 — Stripe como primeiro provedor

- Estado: aceito
- Data: 2026-09-05

## Contexto

O MVP precisa de uma integração real para validar checkout, idempotência,
webhooks, persistência e processamento assíncrono. Começar com uma interface
genérica e vários adapters incompletos esconderia os requisitos concretos que
devem orientar essa abstração.

O projeto também procura minimizar o contato com dados completos de cartão,
funcionar bem para quem ainda está aprendendo integrações de pagamento e ser
reproduzível em ambiente local.

## Decisão

Stripe será o primeiro provedor. A versão `0.1` usará:

- Stripe Checkout na página hospedada;
- Checkout Sessions com `mode=payment`;
- BRL e cartão como o primeiro fluxo vertical;
- Pix depois que o pipeline assíncrono estiver validado;
- SDK oficial `stripe-go`;
- Stripe CLI e ambiente de testes para desenvolvimento local.

O adapter ficará atrás da interface mínima exigida pelos casos de uso, sem
modelar antecipadamente capacidades de outros provedores. Stripe Billing,
Connect, Elements, assinaturas e marketplace não entram nesta decisão.

## Consequências

### Positivas

- O checkout hospedado limita a exposição da aplicação a dados de cartão.
- SDK, sandbox, documentação e ferramentas locais reduzem o custo do primeiro
  exemplo executável.
- Cartão permite validar a fatia vertical antes de adicionar o comportamento
  assíncrono específico de Pix.
- Uma integração concreta produzirá evidências para desenhar uma futura
  interface multi-provedor.

### Custos e limites

- A pessoa que adotar o projeto precisará criar e configurar uma conta Stripe.
- Disponibilidade de Pix e outros métodos depende da elegibilidade e da
  configuração da conta.
- O primeiro desenho refletirá conceitos da Stripe; o domínio deve impedir que
  esses tipos escapem para os casos de uso e para o contrato público.
- A escolha não compara taxas comerciais nem garante adequação da Stripe a todo
  produto ou país.

## Alternativas consideradas

### Mercado Pago

É uma alternativa relevante para Brasil e América Latina e pode se tornar o
segundo adapter. Adiá-lo mantém a primeira entrega pequena e permite validar a
interface existente com uma segunda implementação real no futuro.

### Adapter simulado como primeira entrega

Continuará útil em testes, mas não substituirá o primeiro provedor porque não
expõe limitações reais de API, idempotência, assinatura e entrega de eventos.

## Plano operacional

O escopo detalhado, eventos aceitos, configurações e critérios de teste estão em
[Integração com Stripe](../providers/stripe.md).
