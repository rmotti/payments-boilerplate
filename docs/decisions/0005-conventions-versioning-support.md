# ADR 0005: convenções, versionamento e suporte

- Status: aceito
- Data: 2026-09-05

## Contexto

Um boilerplate público precisa permitir que integradores avaliem o impacto de
uma atualização e que contribuidores produzam mudanças consistentes. O projeto
também precisa limitar suas promessas de manutenção à capacidade real da fase
inicial.

## Decisão

O projeto adotará:

- Convenções de Go, OpenAPI, PostgreSQL, RabbitMQ, observabilidade e testes
  descritas em `docs/conventions.md`.
- Pull requests integrados preferencialmente por squash merge.
- Títulos de pull request no formato Conventional Commits.
- Semantic Versioning com tags `vMAJOR.MINOR.PATCH`.
- Status experimental durante toda a série `v0.x`.
- `PATCH` compatível e `MINOR` potencialmente incompatível durante `v0.x`.
- Apenas a minor mais recente suportada em melhor esforço durante `v0.x`.
- Nenhum SLA de atendimento ou correção nesta fase.
- Suporte somente a combinações de dependências exercitadas pelo CI.
- As duas releases mais recentes de Go ainda mantidas oficialmente.

A API pública inclui o contrato HTTP, configurações, comandos e contratos de
mensagens documentados. Schema do banco, pacotes em `internal/` e detalhes não
documentados não são API pública.

## Consequências positivas

- Releases comunicam o risco de atualização de forma previsível.
- O contrato OpenAPI evolui junto com implementação e testes.
- Contribuições têm formato consistente e automatizável.
- A política de suporte não promete capacidade inexistente.
- Breaking changes precisam ser explícitos e documentados.

## Custos e limitações

- A série `v0.x` pode exigir migração entre versões minor.
- Pessoas usuárias precisam acompanhar changelog e notas de release.
- A minor anterior deixa de receber correções quando uma nova minor é lançada.
- Convenções e compatibilidade precisam ser verificadas continuamente pelo CI.

## Alternativas rejeitadas

- Versões sem semântica definida: dificultariam avaliar atualizações.
- Manter todas as versões `v0.x`: criaria uma obrigação incompatível com a fase
  inicial do projeto.
- Prometer SLA: exigiria estrutura de suporte que o projeto ainda não possui.
- Versionar somente a imagem Docker: não cobriria contrato HTTP, módulo Go,
  configuração e mensagens.

