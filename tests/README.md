# Testes de integracao

Este diretorio fica reservado para testes futuros que atravessem varios pacotes
ou exercitem o fluxo completo da aplicacao.

Os testes de integracao PostgreSQL atuais permanecem ao lado dos repositories,
em `internal/adapters/postgres/repositories`, e sao habilitados por
`TEST_DATABASE_URL`. A CI executa esses testes com detector de race. Ainda nao
existem testes de integracao RabbitMQ; eles entram com o pipeline assincrono das
Fases 3 e 4.
