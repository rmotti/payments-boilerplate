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
- `POST /v1/webhooks/stripe`, público na rede e autenticado pela verificação da
  assinatura sobre os bytes brutos da requisição, com proteção contra replay
  pela janela de tolerância e limite de corpo próprio.
- Inbox e outbox gravadas na mesma transação, com deduplicação garantida pelo
  índice único do evento do provedor e mensagem que referencia o evento em vez
  de copiar o payload da Stripe. Testes de integração cobrem o rollback da
  inbox quando a outbox falha, entregas concorrentes do mesmo evento e a
  unicidade imposta pelo próprio schema.
- Decisão de recepção de webhooks da Fase 3: contrato de resposta por
  situação, que define quando o provedor deve reentregar, e mensagem de
  outbox por referência ao evento, sem copiar o payload do provedor.

### Changed

- Documentação sincronizada com o fim da Fase 2, distinguindo o checkout já
  executável do pipeline assíncrono planejado para a Fase 3 e resumindo o estado
  do roadmap no README.
- Definição de pronto documental e template de pull request adicionados para
  manter documentação, roadmap e changelog alinhados a cada entrega.

[Unreleased]: https://github.com/rmotti/payments-boilerplate/commits/main
