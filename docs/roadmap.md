# Roadmap inicial

O roadmap privilegia uma fatia vertical funcionando antes de abstrações para
múltiplos provedores.

## Fase 0 — Fundação

- [x] Registrar visão, limites e princípios do produto.
- [x] Registrar arquitetura conceitual inicial.
- [x] Definir pessoas desenvolvedoras e equipes como público principal.
- [x] Definir a API headless e o Swagger UI como demonstração do MVP.
- [x] Escolher Go como linguagem da implementação.
- [x] Escolher PostgreSQL como fonte de verdade.
- [x] Escolher RabbitMQ para processamento assíncrono.
- [x] Definir transactional outbox para publicação confiável.
- [x] Adotar Apache License 2.0 para código, documentação e exemplos.
- [x] Definir convenções de código, API, banco, mensageria e Git.
- [x] Adotar Semantic Versioning e definir a série experimental `v0.x`.
- [x] Definir suporte à minor mais recente em melhor esforço e sem SLA.
- [x] Escolher Stripe Checkout como primeiro provedor de pagamentos.

## Fase 1 — Fundação executável

- [ ] Criar módulo Go e comandos `api` e `worker`.
- [ ] Criar contrato OpenAPI e gerar o strict server.
- [ ] Configurar PostgreSQL e RabbitMQ no Docker Compose.
- [ ] Configurar migrations e geração de queries com `sqlc`.
- [ ] Implementar health checks e shutdown gracioso.
- [ ] Publicar Swagger UI e exemplos equivalentes com `curl`.

## Fase 2 — Primeiro pagamento vertical

- [ ] Criar esquema de `Order`, `Payment`, `PaymentAttempt` e `WebhookEvent`.
- [ ] Criar pedido com valor calculado no servidor.
- [ ] Integrar Stripe Checkout hospedado em sandbox com cartão em BRL.
- [ ] Implementar idempotência da criação.
- [ ] Relacionar a sessão da Stripe ao pedido e à tentativa locais.
- [ ] Implementar consulta do estado local do pedido.
- [ ] Documentar configuração e executar o primeiro pagamento de teste.

## Fase 3 — Confirmação assíncrona confiável

- [ ] Validar assinatura usando o corpo bruto da requisição.
- [ ] Persistir inbox e outbox na mesma transação.
- [ ] Deduplicar entregas.
- [ ] Publicar mensagens persistentes com publisher confirms.
- [ ] Consumir com ack manual e concorrência limitada.
- [ ] Implementar retry com backoff e dead-letter queue.
- [ ] Impedir regressões inválidas de estado.
- [ ] Permitir inspeção e reprocessamento seguro.
- [ ] Habilitar Pix e validar a transição assíncrona até o estado final.

## Fase 4 — Qualidade para publicação

- [ ] Cobrir cenários críticos com testes de integração.
- [ ] Adicionar dados e comandos reproduzíveis de sandbox.
- [ ] Verificar que exemplos OpenAPI correspondem às respostas reais.
- [ ] Testar interrupções entre PostgreSQL, relay, RabbitMQ e consumer.
- [ ] Publicar métricas de outbox, filas, retries e DLQ.
- [ ] Revisar logs, secrets e tratamento de dados pessoais.
- [ ] Criar guia de implantação e checklist operacional.
- [ ] Criar guia de contribuição e templates do repositório.
- [ ] Fazer revisão de segurança antes da versão `0.1.0`.

## Depois da versão 0.1

A ordem será definida com base no uso real:

- Reembolsos.
- Assinaturas e entitlements.
- Extração do núcleo e do adaptador para pacotes.
- Segundo provedor para validar a interface existente.
- Observabilidade e reconciliação mais avançadas.

Marketplace, split e payouts exigem uma decisão de produto separada e não são
uma extensão automática deste roadmap.
