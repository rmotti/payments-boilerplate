# Encerramento de entregas

Toda entrega deve terminar com uma revisão da documentação no mesmo pull
request. Documentação não é uma atividade posterior: ela faz parte da definição
de pronto da mudança.

## Durante o desenvolvimento

Antes de implementar, identifique quais contratos, decisões, exemplos e guias
podem ser afetados. Mantenha a documentação próxima do código alterado e evite
abrir uma tarefa separada para corrigir divergências conhecidas.

## Checklist de encerramento

Ao concluir uma entrega, revise estes pontos:

- `api/openapi.yaml`, exemplos e `docs/api.md` quando o contrato HTTP mudar.
- `docs/architecture.md`, `docs/conventions.md` ou um ADR quando houver uma nova
  decisão, responsabilidade ou garantia arquitetural.
- `README.md`, `.env.example` e guias de implantação quando configuração,
  execução local ou operação mudar.
- Documentação do banco e do provedor quando persistência ou integrações
  externas mudarem.
- `docs/roadmap.md`, marcando uma atividade somente depois que seus critérios
  estiverem implementados e validados.
- A tabela de estado no `README.md` quando uma fase começar ou terminar.
- `CHANGELOG.md`, registrando sob `Unreleased` qualquer mudança relevante para
  quem usa ou mantém o projeto.
- Comentários e exemplos que ainda descrevam comportamento antigo ou futuro
  como se já estivesse implementado.

Se um item não se aplicar, isso deve ser indicado no pull request com uma
justificativa curta antes de marcar seu checkbox. A revisão não pode ser
simplesmente ignorada.

## Antes do merge

1. Execute as verificações locais adequadas à mudança.
2. Confirme que código, OpenAPI, exemplos e documentação descrevem o mesmo
   comportamento.
3. Preencha a seção de documentação do template de pull request.
4. Faça o merge somente depois que implementação, testes e documentação forem
   aprovados em conjunto.

O responsável pelo pull request faz a primeira revisão; quem revisa o código
também verifica a documentação. Uma entrega não está concluída enquanto houver
divergência conhecida entre o repositório e seus documentos.
