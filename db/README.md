# Banco de dados

`migrations/` contem migrations SQL executadas pelo Goose. Migrations publicadas
sao imutaveis. `queries/` contem SQL nomeado para geracao com `sqlc`.

GORM nao executa `AutoMigrate`. O schema nasce exclusivamente das migrations.

