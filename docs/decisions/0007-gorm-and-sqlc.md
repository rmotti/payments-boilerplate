# ADR 0007: GORM e sqlc na persistencia

- Status: aceito
- Data: 2026-09-05

## Contexto

O projeto precisa ser simples para adaptar em casos de uso comuns, mas os
fluxos financeiros exigem controle explicito sobre locks, deduplicacao,
transacoes e concorrencia. GORM melhora a produtividade em operacoes CRUD,
enquanto `sqlc` torna o SQL critico visivel e verificavel em compilacao.

## Decisao

GORM sera usado para CRUD comum. `sqlc` sera usado quando a forma da query fizer
parte da garantia do sistema, incluindo inbox, outbox, transicoes condicionais,
`FOR UPDATE`, `SKIP LOCKED` e consultas operacionais.

Ambos usarao o driver pgx sobre `database/sql`. Goose sera a unica autoridade
para migrations. `AutoMigrate` e hooks do GORM com regras de negocio nao serao
usados. Models de persistencia permanecerao separados das entidades de dominio.

Uma unica transacao nao deve misturar GORM e `sqlc` sem uma implementacao
explicita e testada que compartilhe o mesmo `sql.Tx`.

## Consequencias

- CRUD simples exige menos codigo repetitivo.
- SQL sensivel a concorrencia permanece explicito e revisavel.
- O projeto passa a manter dois estilos de acesso a dados, com fronteiras
  documentadas.
- Mapeamentos entre models de persistencia e dominio continuam necessarios.

