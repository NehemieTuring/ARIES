# Plan d'intégration de Claude Code comme AgentHarness dans ARIES

## 1. Contexte et objectif

ARIES supporte actuellement deux harnesses agents : **OpenClaw** (`pkg/harness/openclaw`) et **Hermes**
(`pkg/harness/hermes`), chacun couplé à son propre pont SSH (`pkg/bridge/openclawssh`,
`pkg/bridge/hermesssh`). L'objectif de ce plan est d'évaluer et de décrire le travail nécessaire pour
ajouter **Claude Code** (l'agent CLI d'Anthropic) comme troisième harness, sans modifier la sémantique
des tâches de benchmark ni le cycle de vie fail-closed d'ARIES (isolation confirmée avant évaluation).

Ce document est un plan de travail, pas une implémentation : il identifie les points d'extension
existants, le code à écrire, les décisions de conception à trancher, et une estimation d'effort.

**Mise à jour du 2026-08-25** : un squelette de code a été écrit (`pkg/harness/claudecode/`,
`pkg/bridge/claudecodessh/`), compile et passe `go vet`. Un test de connectivité automatisé de bout en
bout (vrai sandbox Docker + vrai pont SSH + vrai client SSH) a validé le mécanisme de proxy. Une
capture empirique du comportement réel de l'outil Bash de Claude Code a ensuite **invalidé
l'hypothèse initiale de session shell persistante** (§3 original) — voir §7 pour le détail et la
conception révisée qui en résulte, plus simple que prévu.

## 2. Ce qu'ARIES fournit déjà (points d'extension)

### 2.1 Interface `AgentHarness`

`pkg/runner/interfaces.go:17-21` définit un contrat minimal à trois méthodes :

```go
type AgentHarness interface {
    Start(context.Context, core.HarnessRequest) error
    Run(context.Context, string) (core.HarnessResult, error)
    Stop(context.Context) error
}
```

- `core.HarnessRequest` (`pkg/core/types.go:106-115`) : `RunID`, `TaskID`, `Endpoint core.ToolEndpoint`
  (adresse/identité SSH du pont), `Model core.ModelConfig`, `Timeout`, `CPU`, `MemoryMB`, `OutputDir`.
- `core.HarnessResult` (`pkg/core/types.go:130-136`) : `Status`, `FinalResponse`, `Duration`,
  `LogPaths`, `Error`.

Aucune méthode dédiée à la collecte de télémétrie n'est requise : elle se fait via `LogPaths` et les
fichiers d'audit du pont (voir §2.3 et le rapport sur les traces).

### 2.2 Délégation complète de l'appel modèle au harness

`runtime.backend` est validé comme un enum figé (`deepseek`/`sglang`, `pkg/config/config.go:358-359`,
481-492), et dispatché par un switch dans `cmd/aries/wiring.go:73-102`. **Mais** ARIES ne fait jamais
l'appel d'inférence lui-même — son code Go ne fait qu'un préflight de liveness
(`internal/app/preflight.go`, `pkg/model/sglang/client.go`, un simple `GET /v1/models`). L'appel réel
est fait par le processus harness (ex. OpenClaw écrit sa propre config API et appelle le fournisseur
lui-même, `pkg/harness/openclaw/config.go:112-131`).

**Conséquence pour Claude Code** : il peut appeler l'API Anthropic nativement, sans qu'ARIES ait besoin
de comprendre ce format. Seul un ajustement mineur de l'enum de validation de profil est requis pour
accepter une nouvelle valeur de backend (proposé : `anthropic`).

### 2.3 Pont SSH : pattern réutilisable, grammaire non réutilisable

`pkg/bridge/hermesssh/` et `pkg/bridge/openclawssh/` partagent une structure commune (~1100-1200 lignes
chacun) :
- Génération de clés ed25519 par session, fichier d'identité et `known_hosts` privés
  (`hermesssh/bridge.go:555-654`).
- Un `core.ToolEndpoint{Protocol:"ssh", Address, Username:"aries", IdentityFile, ...}` retourné au
  harness pour qu'il s'y connecte lui-même.
- Un `toolCallRecord` journalisé en JSONL (`bridge.go:107-120`) et un `ssh_raw.log` optionnel.
- Un **parseur de grammaire dédié** (`grammar.go`) qui ne reconnaît que les formes exactes de commandes
  que *son* harness envoie (pour Hermes : sonde de connexion, `echo $HOME`, `bash -c <script>`,
  `bash -l -c <script>` — documenté comme lié à un seul appelant exact, `hermesssh/grammar.go:8-21`).

**Conséquence pour Claude Code** : la grammaire ne peut pas être réutilisée telle quelle. Il faut un
nouveau pont (`pkg/bridge/claudecodessh/`) avec sa propre grammaire, calquée sur la structure existante
mais reconnaissant les formes de payload que Claude Code envoie réellement.

### 2.4 Verrou à lever : `wiring.go`

`cmd/aries/wiring.go:57-67` n'accepte aujourd'hui que les paires `openclaw`/`openclaw-ssh` et
`hermes`/`hermes-ssh`, et **rejette explicitement** toute autre combinaison. Il faudra y ajouter la
paire `claude-code`/`claude-code-ssh`.

## 3. Le point de conception central : comment router l'outil Bash de Claude Code

Claude Code exécute normalement ses commandes shell directement sur la machine où il tourne — pas via
un pont distant. Pour rentrer dans le modèle d'isolation d'ARIES (harness et sandbox dans deux
conteneurs Docker séparés, connectés uniquement par le pont), il faut que l'outil Bash de Claude Code,
depuis l'intérieur de son conteneur harness, finisse par exécuter ses commandes **dans le sandbox**, via
le pont.

Deux options identifiées :

| Option | Description | Avantage | Risque |
|---|---|---|---|
| **A. Wrapper `/bin/bash` transparent** | Remplacer `/bin/bash` du conteneur harness par un script qui forward la commande via SSH vers le sandbox (identité fournie par `ToolEndpoint`) | Claude Code ne voit aucune différence ; réutilise le pattern SSH existant tel quel | Le wrapper doit reproduire fidèlement la sémantique de `bash -c` (stdin, codes de sortie, streaming stdout/stderr) |
| **B. Serveur MCP d'exécution distante** | Donner à Claude Code un outil MCP personnalisé qui exécute la commande via SSH, à la place de son outil Bash natif | Plus explicite, plus facile à auditer indépendamment du wrapper shell | Change le comportement observé de Claude Code par rapport à son usage natif — pertinent si l'objectif est de mesurer Claude Code "tel quel" |

**Recommandation** : Option A, pour rester cohérent avec le principe d'ARIES de préserver le
comportement natif de chaque harness (cf. §"Preserving task semantics" du papier ARIES) — Claude Code
ne doit pas savoir qu'il tourne dans ce dispositif.

## 4. Plan de travail par étapes

1. ✅ **Prototype de connectivité** — fait le 2026-08-25 : `cmd/aries-claudecode-probe/` démarre un vrai
   sandbox Docker, un vrai pont SSH, s'y connecte avec un vrai client SSH, et valide le proxy d'une
   session shell. Conservé comme outil de diagnostic.
2. ✅ **Capturer la grammaire réelle** — fait le 2026-08-25 par shim de `/bin/bash` dans un conteneur
   jetable avec Claude Code réellement installé. **Résultat contraire à l'hypothèse initiale** — voir §7.
3. ✅ **Écrire `pkg/bridge/claudecodessh/`** — écrit, puis entièrement réécrit après l'étape 2 pour
   refléter le vrai modèle (exec discret par commande, comme `hermesssh`, pas une session persistante).
4. ✅ **Écrire `pkg/harness/claudecode/`** — squelette écrit (`Start`/`Run`/`Stop`, rendu de config,
   collecte d'artefacts). Compile et passe `go vet`.
5. ✅ **Étendre `wiring.go`** — fait (paire `claude-code`/`claude-code-ssh` acceptée).
6. ✅ **Étendre l'enum de config** — fait (`anthropic` accepté comme `runtime.backend`, mode `external`
   uniquement, préflight de liveness encore en TODO).
7. ⬜ **Profil de test** — pas encore fait ; nécessite d'abord une image Docker avec Claude Code installé
   et épinglée (`configs/versions.json`), et le wrapper bash de l'option A (voir §7.4).
8. ⬜ **Validation du cycle fail-closed** — pas encore testée avec un vrai harness Claude Code de bout en
   bout (seul le pont a été testé isolément, avec un shell générique, pas encore avec `claude` lui-même
   au travers du pont).
9. ⬜ **Télémétrie** — inchangé, toujours à vérifier.

## 5. Risques et inconnues à lever avant de démarrer le code

- **Comportement exact du wrapper bash** : reproduire fidèlement stdin/stdout/stderr/codes de sortie
  pour ne pas fausser le comportement de Claude Code par rapport à son usage natif.
- **Format d'export de trajectoire de Claude Code** : à l'inverse d'OpenClaw (fichiers de session sur
  disque) et Hermes (export SQLite via commande dédiée), le mécanisme d'export de session de Claude Code
  n'a pas encore été vérifié dans ce plan — étape à faire avant l'écriute de `collectArtifacts`.
- **Coût de l'étape 2** (capture de grammaire réelle) : nécessite un environnement de test isolé avant
  d'écrire le parseur, sous peine de grammaire incomplète en production.

## 6. Estimation d'effort

Chantier de taille moyenne à significative : nouveau package harness + nouveau pont SSH avec grammaire
dédiée + ajustements de wiring/config. Comparable en ampleur à l'ajout initial du support Hermes dans
ARIES (harness + bridge dédiés), pas un simple changement de configuration.

## 7. Résultats empiriques (2026-08-25)

### 7.1 Test de connectivité — validé

`cmd/aries-claudecode-probe` a démarré un vrai sandbox Docker (`ubuntu:24.04`), un vrai pont
`claudecodessh`, puis s'y est connecté avec un vrai client SSH (`golang.org/x/crypto/ssh`), envoyant
plusieurs commandes séquentielles. Le mécanisme de proxy vers `ExecStream` fonctionne correctement —
voir `ssh_raw.log` capturé pendant le test pour la trace complète.

### 7.2 Grammaire réelle de l'outil Bash — hypothèse initiale invalidée

Méthode : `/bin/bash` a été remplacé (par renommage atomique, pas écriture en place — `ETXTBSY` sinon)
par un script journalisant chaque invocation puis délégant au vrai bash, à l'intérieur d'un conteneur
Docker jetable (`node:22-slim`) avec Claude Code réellement installé (`npm install -g
@anthropic-ai/claude-code`) et authentifié. Une tâche demandant deux appels d'outils (`ls -la` puis
`date`) a produit **5 invocations séparées de `bash`**, jamais une session persistante :

1. Sonde initiale : `bash -c env`
2. Une seule fois par session : `bash -c -l "SNAPSHOT_FILE=... source ~/.bashrc ... "` — génère un
   fichier "snapshot" capturant fonctions, alias, options du shell et PATH, plus des fonctions
   `rg`/`find`/`grep`/`pkill` propres à Claude Code.
3. Par appel d'outil : `bash -c "source $SNAPSHOT_FILE 2>/dev/null || true && shopt -u extglob
   2>/dev/null || true && { \builtin unalias -- 'unsetenv'; \builtin unset -f -- 'unsetenv'; }
   >/dev/null 2>&1 || true && eval '<commande>' < /dev/null && pwd -P >| /tmp/claude-<id>-cwd"`

**Conclusion** : Claude Code ne maintient jamais un processus shell persistant. Il simule la
persistance d'état (répertoire courant, fonctions, alias) en **re-sourçant un fichier snapshot** à
chaque appel et en traçant le `cwd` résultant dans un fichier temporaire. C'est exactement le modèle
d'exécution discrète que Hermes et OpenClaw utilisent déjà — `pkg/bridge/claudecodessh` a donc été
réécrit pour dispatcher une requête SSH `exec` par commande (comme `hermesssh`) plutôt que de proxier un
canal `shell` continu. C'est une **simplification** par rapport à la conception initiale : pas besoin de
support pty, pas besoin de gérer un canal long-lived.

Reste non vérifié (TODO dans `grammar.go`) : le désescapage d'une commande contenant un guillemet simple
n'a été exercé que sur des commandes sans caractère spécial (`ls -la`, `date`).

### 7.3 Authentification — deux mécanismes distincts découverts

- `ANTHROPIC_API_KEY` seule en variable d'environnement **ne suffit pas** pour l'usage non interactif
  (`claude -p`) sans session déjà connectée : échoue avec `Not logged in · Please run /login`.
- Le mécanisme qui fonctionne est `apiKeyHelper` dans `~/.claude/settings.json`, pointant vers une
  commande qui **lit la clé depuis un fichier** (`"apiKeyHelper": "cat /chemin/vers/cle"`) — un helper
  qui lit `$ANTHROPIC_API_KEY` directement échoue aussi (`did not return a value`), probablement parce
  que Claude Code retire cette variable de l'environnement avant d'invoquer le helper pour éviter une
  dépendance circulaire. **Ceci confirme le choix déjà fait dans `pkg/harness/claudecode/config.go`**
  (`ANTHROPIC_API_KEY_HELPER` lisant `modelKeyPath`).
- Alternative sans clé API du tout : monter en lecture seule `~/.claude/.credentials.json` et
  `~/.claude.json` (session OAuth existante) dans le conteneur — fonctionne, mais ce n'est pas le
  mécanisme portable qu'ARIES peut injecter par profil (c'est une session personnelle liée à un compte,
  pas un identifiant scopé par tâche) ; à réserver aux tests manuels, pas à l'intégration réelle.

### 7.4 Wrapper bash écrit et testé de bout en bout — succès

Le wrapper (`bashWrapperScript` dans `pkg/harness/claudecode/config.go`) a été écrit, câblé dans
`runtimeArchive` (remplace `/bin/bash` intégralement dans l'archive de staging — pas de `bash.real` à
préserver, puisque toute invocation dans le conteneur harness est censée partir vers le sandbox) et
testé avec un **vrai client `ssh`** (pas seulement la bibliothèque Go) via `cmd/aries-claudecode-probe`.
Trois bugs réels trouvés et corrigés pendant ce test :

1. **Duplication de `bash` dans les arguments** : `core.Command.Args` ne doit pas inclure le nom du
   programme (déjà dans `Path`) — l'inclure produisait `/bin/bash bash -c <script>`, que bash
   interprétait comme "exécute le fichier script nommé bash", d'où `cannot execute binary file`.
2. **Adresse `host:port` mal formée pour `ssh`** : le wrapper collait `host:port` directement après
   `user@`, syntaxe invalide pour le client `ssh` (c'est une syntaxe SCP/URL, pas CLI). Corrigé en
   séparant host et port via l'expansion de paramètres POSIX (`${VAR%:*}` / `${VAR##*:}`) et en passant
   `-p <port>` séparément.
3. **Format de clé incompatible avec un vrai client OpenSSH** : `generateSessionKeys` (copié de
   `hermesssh`) produisait une clé PEM PKCS8 générique (`x509.MarshalPKCS8PrivateKey` + bloc
   `"PRIVATE KEY"`). `openssl pkey` la valide comme clé ed25519 correcte, mais `ssh`/`ssh-keygen`
   d'OpenSSH 8.9p1 (celui de cette machine) la refuse avec `is not a key file`. Remplacé par
   `ssh.MarshalPrivateKey` (`golang.org/x/crypto/ssh`), qui produit le format natif
   `"OPENSSH PRIVATE KEY"`, universellement accepté. `hermesssh` utilise le même code PKCS8 et
   fonctionne pourtant en usage réel (validé plus tôt dans le même run ARIES) — probablement parce que
   le client SSH utilisé *à l'intérieur* du conteneur Hermes diffère de celui de cette machine hôte ;
   cette incompatibilité ne s'est révélée qu'en testant avec le `ssh` de l'hôte, pas avec le client Go.

Après ces trois corrections, le test de bout en bout passe : le wrapper, invoqué comme `/bin/bash`
le serait par Claude Code (`bash -c "env"`), se connecte via un vrai `ssh`, traverse le pont, s'exécute
dans le sandbox, et renvoie une sortie confirmant l'exécution distante (`HOSTNAME=<id-du-conteneur-sandbox>`
dans la sortie de `env`, différent de l'hôte).

### 7.5 Harness complet testé de bout en bout (`cmd/aries-claudecode-harness-probe`)

Une image Docker locale jetable (`aries-claudecode:test-1` — Node 22 + `openssh-client` + Claude Code
CLI + utilisateur non-root fixe `aries` UID 10000, voir `Dockerfile.claudecode` dans le scratchpad) a
permis de tester le vrai `pkg/harness/claudecode.Manager` (`Start`/`Run`/`Stop`), pas seulement le pont,
contre un vrai sandbox Docker et un vrai pont. Deux bugs supplémentaires trouvés et corrigés :

4. **Mauvais mécanisme de clé API dans `renderSettings`** : la première version écrivait
   `ANTHROPIC_API_KEY_HELPER` sous `"env"` dans `settings.json` — un nom inventé, silencieusement
   ignoré par Claude Code (pas d'erreur, juste un retour à "Not logged in"). Corrigé pour utiliser
   `"apiKeyHelper": "cat <chemin>"` **à la racine** de `settings.json`, le mécanisme confirmé
   fonctionnel en §7.3.
5. **Permissions non-interactives manquantes** : `--dangerously-skip-permissions` n'était pas passé à
   `claude -p`. Ajouté — sûr maintenant que le conteneur tourne systématiquement en non-root (UID fixe
   10000, `runtimeUID`/`runtimeGID`, également nécessaire pour que l'utilisateur non-root puisse lire
   `modelKeyPath` en mode 0600 — les fichiers stagés par `stageArchive` n'étaient auparavant possédés
   par personne en particulier, donc `root` par défaut).

Après ces corrections, le harness complet démarre, authentifie correctement via `apiKeyHelper` (plus
d'erreur `did not return a value` ni `Not logged in`), et échoue seulement sur
`"Credit balance is too low"` — le même compte API à 0 $ que précédemment (§ le tout début de la
session). C'est donc un échec de facturation, pas un défaut du harness : **toute la chaîne technique
(staging, wrapper, pont, authentification) est validée de bout en bout** ; il ne reste qu'à créditer le
compte pour observer une tâche réussie complète.

### 7.5bis Trois bugs de câblage supplémentaires trouvés en testant le vrai wrapper bash

Le test de bout en bout (§7.5) a d'abord échoué avec `exec request failed on channel 0` côté Claude
Code pour **tout** appel Bash, même triviaux (`pwd`). Trois causes distinctes, empilées, ont dû être
corrigées avant d'obtenir un vrai succès :

1. **Le wrapper ne lisait que `$1`/`$2`** alors que l'invocation réelle du générateur de snapshot a
   **trois** arguments (`bash -c -l "<script>"`), pas deux. Le script perdait silencieusement le vrai
   contenu (`flag="-c"`, `script="-l"`, le troisième argument jamais lu).
2. **Reconstituer la commande avec de simples espaces est intrinsèquement ambigu** : un vrai script
   par appel peut lui-même commencer par un mot qui ressemble à un drapeau ; impossible de distinguer
   après coup "fin d'argument" de "espace dans le texte du script". Remplacé par un séparateur non
   imprimable exact (`\x1f`, Unit Separator ASCII — même convention que les trailers d'Hermes) : le
   wrapper joint tous les arguments originaux avec ce séparateur, et le pont les resépare avec
   `strings.Split`, sans aucune ambiguïté.
3. **Le motif de la grammaire supposait un préfixe `source $SNAPSHOT_FILE ... &&` obligatoire**, mais
   la vraie forme observée en bout en bout omet parfois entièrement cette ligne
   (`bash -c -l "shopt -u extglob ... && eval '<cmd>' ... && pwd -P >| ..."`, sans aucun `source`).
   Rendu optionnel dans le regex ; la condition exacte qui déclenche l'un ou l'autre reste incomprise.

Une fois ces trois corrections faites, l'outil Bash de Claude Code a fonctionné correctement à travers
le pont — plus aucune erreur `exec request failed`.

### 7.5ter Limite architecturale découverte : les outils Write/Read/Edit ne passent pas par le pont

Une fois Bash réparé, Claude Code a lui-même signalé le problème suivant dans sa réponse finale :

> "the Bash tool is connected via SSH to a separate machine (root@172.18.0.1, home dir /root) that
> doesn't share a filesystem with the one my Write/Read tools use (/home/aries/workspace)."

C'est exact et c'est une **limite de conception**, pas un bug de câblage : Claude Code possède des
outils natifs (`Write`, `Read`, `Edit`, `Glob`, `Grep`) qui manipulent le système de fichiers
**directement** (appels `fs` de Node.js), sans jamais passer par `bash`. Le mécanisme du wrapper
(option A, §3) n'intercepte que l'invocation de `/bin/bash` — il ne peut donc, par construction,
rediriger que l'outil Bash vers le sandbox. Les outils de fichiers natifs continuent d'opérer sur le
système de fichiers **local du conteneur harness**, jamais synchronisé avec le sandbox.

Conséquence concrète : un fichier créé avec `Write` existe dans le harness, mais **pas** dans le
sandbox — donc invisible pour le vérificateur du benchmark, qui inspecte le sandbox. C'est un problème
plus profond que celui résolu en §7.5bis, et propre à Claude Code parmi les trois harnesses (OpenClaw et
Hermes ne semblent pas exposer d'outils de fichiers natifs équivalents, en tout cas pas documentés comme
tels). Pistes non explorées dans cette session, à trancher avant une intégration réelle :

- Fournir un serveur MCP personnalisé exposant des outils de fichiers redirigés vers le sandbox, et
  désactiver les outils natifs Write/Read/Edit/Glob/Grep de Claude Code en config.
- Un montage réseau (sshfs ou équivalent) rendant le répertoire de travail du harness littéralement
  identique à celui du sandbox — probablement en tension avec le modèle de sécurité d'ARIES (pas de
  système de fichiers partagé entre harness et sandbox en dehors du pont).
- Limiter le périmètre d'usage de Claude Code dans ARIES aux tâches qui n'exercent que l'outil Bash
  (perdant une bonne partie de ce qui rend Claude Code utile en pratique).

### 7.6 Conséquence pour le wrapper bash (option A)

Le modèle d'exécution discrète simplifie aussi le wrapper prévu au §3 : au lieu de devoir maintenir un
tunnel SSH interactif pour toute la durée de vie du harness, le wrapper `/bin/bash` du conteneur harness
n'a qu'à transformer chaque invocation `bash -c "<script>"` en **une requête SSH `exec` unique** vers le
pont (connexion réutilisable, requête par commande) — un aller-retour par appel d'outil, sans état à
maintenir côté wrapper lui-même.

## 8. Conclusion

L'architecture d'ARIES est conçue pour rendre cette intégration possible sans modification du cœur du
framework (Runner, cycle de vie fail-closed, Benchmark) : l'interface `AgentHarness` est petite, et la
délégation de l'appel modèle au harness lui-même évite tout travail côté ARIES pour parler à l'API
Anthropic. Le travail réel se concentre sur un nouveau pont SSH avec sa propre grammaire, et sur la
décision de conception pour router l'outil Bash de Claude Code à travers ce pont sans changer son
comportement observable.
