# Repositories PostgreSQL

Implementacoes de persistencia para os casos de uso. Os repositories de pedido e
pagamento usam GORM, transacoes curtas e constraints do schema para garantir
idempotencia e impedir checkouts concorrentes.

O repository de webhooks usa queries geradas por `sqlc` sobre `database/sql`,
porque a forma dessas queries faz parte da garantia: o `ON CONFLICT DO NOTHING`
e o que deduplica uma reentrega, e o inbox e o outbox precisam ser gravados na
mesma transacao. As duas tabelas sao novas, entao nenhuma transacao mistura os
dois estilos de acesso, como pede o ADR 0007.

O polling com lock do relay e as transicoes condicionais do consumer, ainda na
Fase 3, seguirao o mesmo caminho.
