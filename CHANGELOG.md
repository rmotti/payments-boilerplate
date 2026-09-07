# Changelog

Todas as mudanças relevantes deste projeto serão documentadas neste arquivo.

O formato é inspirado em [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
e o projeto segue [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Visão, escopo, arquitetura e roadmap iniciais.
- Contrato conceitual da API headless.
- Métricas prioritárias do produto e da operação.
- Política de segurança e guia de contribuição.
- Apache License 2.0.
- Decisões arquiteturais para Go, PostgreSQL, RabbitMQ e transactional outbox.
- Convenções, política de versionamento e política de suporte.
- Stripe Checkout como primeiro provedor e plano de integração em sandbox.
- Esquema de `orders`, `payments`, `payment_attempts` e `webhook_events`.
- `POST /v1/orders` com valor e moeda calculados no servidor a partir de um
  catálogo fixo, header `Idempotency-Key` obrigatório e erros JSON com código
  estável e identificador de correlação.
- Modelo de acesso self-hosted e single-integrator, com autenticação por API key
  protegendo as rotas de negócio por padrão, rotação de credenciais e health
  público.
- Stripe Checkout hospedado para cartão em BRL, com sessão ligada a `Payment` e
  `PaymentAttempt`, proteção contra checkouts concorrentes e persistência da URL
  antes da resposta, validado por um pagamento completo no sandbox.
- Idempotência transparente em criação de pedidos e checkouts, além de
  `GET /v1/orders/{orderId}` para consultar o estado local.

[Unreleased]: https://github.com/rmotti/payments-boilerplate/commits/main
