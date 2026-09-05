# Métricas prioritárias

O boilerplate prioriza correção financeira e tempo de integração antes de
throughput bruto. Os valores abaixo são referências para o ambiente de
desenvolvimento, não um SLA oferecido pelo projeto.

## Experiência de adoção

| Métrica | Referência inicial |
| --- | ---: |
| API disponível após clone e configuração | até 10 minutos |
| Primeiro pagamento em sandbox | até 20 minutos |
| Endpoints públicos presentes no OpenAPI | 100% |
| Preparação manual do banco e do broker | nenhuma |
| Exemplos documentados verificados em CI | 100% |

`Time to First Successful Payment` é a principal métrica de utilidade: mede o
tempo entre uma pessoa obter o repositório e confirmar o primeiro pagamento em
sandbox.

## Correção

- Cobranças duplicadas causadas pela aplicação: zero.
- Efeitos duplicados após redelivery: zero.
- Eventos válidos perdidos em testes de falha: zero.
- Transições inválidas ou regressivas: zero.
- Divergências silenciosas entre pedido e pagamento: zero.

Não será prometida entrega exatamente uma vez. A arquitetura assume entrega pelo
menos uma vez e garante idempotência dos efeitos observáveis.

## Latência e backlog

| Métrica | Referência inicial |
| --- | ---: |
| Aceite durável do webhook, p95 | abaixo de 250 ms |
| Processamento completo do evento, p95 | abaixo de 5 s |
| Processamento completo do evento, p99 | abaixo de 30 s |
| Idade da mensagem mais antiga no outbox | abaixo de 60 s |
| Idade da mensagem pronta mais antiga no RabbitMQ | abaixo de 60 s |

A latência da chamada ao provedor será medida separadamente da latência interna,
pois não está sob controle da aplicação.

## Instrumentação

```text
http.server.request.duration
payment.provider.request.duration
payment.provider.request.errors
payment.webhook.received
payment.webhook.invalid_signature
payment.webhook.duplicate
payment.webhook.processing.duration
payment.webhook.processing.failures
payment.state.transitions
payment.idempotency.replays
payment.idempotency.conflicts
outbox.pending
outbox.oldest.age
rabbitmq.publish.duration
rabbitmq.publish.failures
rabbitmq.redeliveries
rabbitmq.consumer.duration
rabbitmq.consumer.failures
rabbitmq.retry.count
rabbitmq.dlq.depth
database.pool.usage
```

Métricas não devem usar IDs de pedido, pagamento, cliente ou evento como labels.
Esses valores têm alta cardinalidade e devem aparecer apenas em traces ou logs
controlados por identificadores de correlação.

## Testes de desempenho

Cada resultado publicado deve informar máquina, versão do Go, PostgreSQL,
RabbitMQ, configuração e cenário. Não serão usados benchmarks isolados de
framework para afirmar capacidade do sistema completo.

Os primeiros testes devem avaliar:

- Criação e consulta concorrente de pedidos.
- Rajada de webhooks válidos, inválidos e duplicados.
- Crescimento e recuperação do outbox durante indisponibilidade do RabbitMQ.
- Redelivery após interrupção do worker.
- Tempo para drenar backlog com diferentes valores de concorrência e prefetch.

