# Migrations

Goose e a unica autoridade de schema; `AutoMigrate` nao e usado. Use nomes
sequenciais, SQL explicito e secoes `-- +goose Up` e `-- +goose Down`.

Migrations publicadas sao imutaveis. Correcoes entram em uma nova migration,
e mudancas destrutivas seguem a sequencia `expand`, `migrate`, `contract`.

| Arquivo | Conteudo |
| --- | --- |
| `00001_foundation.sql` | Marcador da fundacao, sem objetos de schema. |
| `00002_payments.sql` | `orders`, `payments`, `payment_attempts` e `webhook_events`. |
