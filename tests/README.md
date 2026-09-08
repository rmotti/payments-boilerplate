# Testes de integração e ponta a ponta

Os testes de integracao vivem ao lado dos adapters que exercitam e sao
habilitados por variaveis de ambiente, de modo que a suite passa sem infra:

- PostgreSQL, em `internal/adapters/postgres/repositories`, com
  `TEST_DATABASE_URL`.
- RabbitMQ, em `internal/adapters/rabbitmq`, com `TEST_RABBITMQ_URL`.

A CI executa esses testes com detector de race.

## Suíte ponta a ponta

`tests/e2e` usa a build tag `e2e` e inicia os mesmos pacotes de composição da
API e do worker. A suíte injeta somente a porta `PaymentProvider`; a verificação
de webhook permanece o adapter Stripe real e recebe eventos assinados
localmente. Requests e responses HTTP também são validados contra
`api/openapi.yaml`.

Os nomes de exchange e filas são constantes, portanto a suíte inteira é
serial: os testes não usam `t.Parallel()` e este pacote não pode ser executado
ao mesmo tempo que os testes de adapter RabbitMQ contra o mesmo broker.

A suíte aplica migrations e realiza cleanup destrutivo apenas sobre estas
tabelas conhecidas: `outbox_events`, `webhook_events`, `payment_attempts`,
`payments` e `orders`. No broker, purga somente `payments.webhooks`,
`payments.webhooks.dlq` e as filas dos tiers de retry usados pelo harness.
Antes de qualquer uma dessas operações, três condições são obrigatórias:

- `E2E_ALLOW_DESTRUCTIVE_CLEANUP=YES` exatamente;
- URLs apontando para `localhost` ou endereço IP de loopback;
- usuário, banco e vhost iguais aos do Compose local documentado.

Execução contra o Compose local (a porta PostgreSQL abaixo acompanha o job e2e
dedicado; ajuste somente a porta se o serviço local foi publicado em outra):

```sh
E2E_ALLOW_DESTRUCTIVE_CLEANUP=YES \
E2E_DATABASE_URL='postgres://payments:payments_local@127.0.0.1:55432/payments?sslmode=disable' \
E2E_RABBITMQ_URL='amqp://payments:payments_local@127.0.0.1:5672/' \
make test-e2e
```

Sem as duas URLs, os cenários dependentes de infraestrutura são pulados, mas a
compilação do harness e os testes das proteções de cleanup continuam rodando.
Uma URL remota ou uma flag ausente causa recusa explícita, nunca um fallback.

Toda sincronização assíncrona usa polling com deadline. Ao estourar o prazo, o
erro inclui os IDs envolvidos, o último estado observado e os logs capturados
da composição. Há ainda um cenário black-box que compila e executa o binário do
worker para provar carregamento de configuração, `SIGTERM` e shutdown real de
processo.

## Suítes de contrato e chaos

`make test-contract` cruza requests e responses reais, exemplos marcados e o
manifesto completo de operação/status com `api/openapi.yaml`. Ela não requer
infraestrutura externa e roda a cada pull request.

`make test-chaos` executa as janelas determinísticas de falha e o próprio proxy
TCP. `make test-chaos-long` adiciona indisponibilidade e recuperação reais,
backlog e a matriz de concorrência/prefetch; exige PostgreSQL e RabbitMQ
exclusivos, `TEST_DATABASE_URL`, `TEST_RABBITMQ_URL` e o consentimento explícito
`CHAOS_DESTRUCTIVE_ALLOWED=1`. A suíte prolongada aceita publicação ou entrega
duplicada, mas exige efeitos finais idempotentes, e registra máquina, versões,
configuração, volume, duração, p50 e p95 no log preservado pela CI.

A suíte prolongada roda por agendamento e despacho manual. O workflow de
release repete o gate no próprio SHA da tag antes de publicar artefatos; um
resultado aprovado de outro commit não libera a versão.
