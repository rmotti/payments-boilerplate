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
- Relay do outbox executando dentro do `worker`. Ele reserva lotes por lease em
  transações curtas, publica sem manter transação aberta durante a chamada ao
  broker e só marca uma mensagem como publicada após o publisher confirm. Mais
  de uma instância de worker é segura sem coordenação externa, e um relay que
  morre devolve seu trabalho quando o lease expira.
- Publicação `mandatory`, para que uma mensagem que nenhuma fila recebeu seja
  reportada em vez de contar como publicada apesar do confirm.
- Política de falhas do relay: indisponibilidade do broker, timeout, confirm
  negativo e mensagem não roteada são transitórios e reagendados com backoff
  exponencial e jitter, sem nunca descartar a mensagem. Apenas erros
  classificados como permanentes abandonam uma mensagem, e o limite de
  tentativas apenas dispara alerta.
- Reconexão AMQP sob demanda, com prazo de socket cobrindo rediscagem,
  handshake, RPCs de preparação, envio e espera do confirm. O driver não
  interrompe todas essas operações apenas com o cancelamento do `context`, por
  isso o limite permanece instalado no transporte durante a tentativa inteira.
  O worker também passa a subir com o broker indisponível, já que as mensagens
  estão duráveis no PostgreSQL.
- Prazos do lease calculados pelo PostgreSQL, que é o único relógio
  compartilhado entre instâncias, e dono do lease identificando o processo e
  não a máquina, para que dois workers no mesmo host não colidam. Uma janela
  monotônica local, iniciada antes do lease, limita a publicação sem comparar o
  relógio absoluto do banco com o relógio do worker e reserva tempo para a
  liquidação.
- Perda de lease detectada e reportada em vez de contabilizada como sucesso,
  já que a mensagem será publicada novamente por outra instância.
- Topologia do RabbitMQ declarada de forma idempotente pela aplicação, com
  exchange `payments.events`, fila durável `payments.webhooks` e dead-letter
  criada desde o início, já que argumentos de fila são imutáveis.
- Decisão de execução do relay e da topologia do broker, registrada no ADR 0012.
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
