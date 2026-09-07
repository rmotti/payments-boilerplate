# Repositories PostgreSQL

Implementacoes de persistencia para os casos de uso. Os repositories atuais
usam GORM, transacoes curtas e constraints do schema para garantir idempotencia
e impedir checkouts concorrentes.

Queries geradas por `sqlc` serao usadas quando locks, polling, deduplicacao ou
transicoes condicionais precisarem de SQL explicito, especialmente no pipeline
de inbox/outbox da Fase 3.
