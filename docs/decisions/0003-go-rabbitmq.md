# ADR 0003: Go e RabbitMQ no MVP

- Status: aceito
- Data: 2026-09-05

## Contexto

O projeto será uma API headless implantável de forma independente. Além de
responder chamadas HTTP, precisa aceitar webhooks rapidamente e processá-los de
forma concorrente, durável e recuperável desde a primeira versão.

## Decisão

A implementação usará Go. O contrato OpenAPI será a fonte de verdade e gerará
interfaces estritas de servidor. PostgreSQL será a fonte de verdade dos estados
de negócio, com `pgx` e queries tipadas geradas por `sqlc`.

RabbitMQ será uma dependência obrigatória do MVP. Webhooks aceitos serão
registrados em uma inbox e produzirão registros de outbox na mesma transação. Um
relay publicará mensagens persistentes usando publisher confirms. Consumers
usarão ack manual e só confirmarão depois de persistir seus efeitos.

API, outbox relay e consumer compartilharão o mesmo módulo Go. Inicialmente
serão expostos como dois processos: `api` e `worker`. Isso não implica separar o
projeto em múltiplos microsserviços.

## Semântica de entrega

O sistema assume entrega pelo menos uma vez. Uma mensagem pode ser republicada
se houver queda depois do publisher confirm e antes da atualização do outbox, ou
ser entregue novamente se o consumer cair antes do ack. Constraints, transações
e handlers idempotentes impedem efeitos duplicados.

## Consequências positivas

- Processamento assíncrono e recuperável desde a versão inicial.
- Concorrência explícita e controlável no worker.
- Separação clara entre aceite do webhook e aplicação dos efeitos.
- Topologia semelhante à encontrada em integrações reais.
- Binários independentes e simples de implantar.

## Custos e limitações

- PostgreSQL e RabbitMQ passam a ser dependências obrigatórias.
- O ambiente local e os testes de integração ficam mais complexos.
- Transactional outbox, publisher confirms, ack, retry e DLQ precisam ser
  implementados e testados corretamente.
- Operadores precisam monitorar broker, outbox, backlog e mensagens mortas.
- A presença de RabbitMQ não remove a necessidade de idempotência.

## Alternativas rejeitadas neste momento

- Goroutines sem persistência: perdem trabalho quando o processo encerra.
- PostgreSQL como única fila: reduziria infraestrutura, mas não atende à decisão
  de exercitar mensageria dedicada desde o MVP.
- Kafka: orientado a event streaming e replay por múltiplos consumidores, além
  das necessidades atuais.
- Vários microsserviços: aumentariam falhas distribuídas sem ampliar o valor do
  primeiro fluxo.

