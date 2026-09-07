# Testes de integracao

Este diretorio fica reservado para testes futuros que atravessem varios pacotes
ou exercitem o fluxo completo da aplicacao.

Os testes de integracao vivem ao lado dos adapters que exercitam e sao
habilitados por variaveis de ambiente, de modo que a suite passa sem infra:

- PostgreSQL, em `internal/adapters/postgres/repositories`, com
  `TEST_DATABASE_URL`.
- RabbitMQ, em `internal/adapters/rabbitmq`, com `TEST_RABBITMQ_URL`.

A CI executa esses testes com detector de race.
