# Zig — multi-threading & concurrence

## Modèle 1:1 — pas de scheduler runtime

Contrairement à Go (M:N), Zig utilise un modèle **1:1** : un thread Zig = un thread OS. Pas de scheduler intermédiaire, pas de goroutines. Le système d'exploitation gère entièrement le placement des threads sur les cœurs.

```
Thread Zig 1 ──► Thread OS 1 ──► Core 0
Thread Zig 2 ──► Thread OS 2 ──► Core 1
Thread Zig 3 ──► Thread OS 3 ──► Core 2
```

C'est plus bas niveau, plus prévisible, mais tout est à gérer manuellement.

---

## Threads — `std.Thread`

```zig
const std = @import("std");

fn worker(arg: u32) void {
    std.debug.print("thread reçoit : {}\n", .{arg});
}

pub fn main() !void {
    const t = try std.Thread.spawn(.{}, worker, .{42});
    t.join();   // attendre la fin du thread
}
```

**Options de spawn** : `.stack_size` permet de définir la taille de pile (défaut OS, ~1–8 MB).

```zig
const t = try std.Thread.spawn(.{ .stack_size = 2 * 1024 * 1024 }, worker, .{});
```

---

## Primitives de synchronisation

### Mutex

```zig
var mutex = std.Thread.Mutex{};

mutex.lock();
defer mutex.unlock();
// section critique
```

### RwLock — lectures concurrentes, écriture exclusive

```zig
var rwlock = std.Thread.RwLock{};

// plusieurs goroutines peuvent lire en même temps
rwlock.lockShared();
defer rwlock.unlockShared();

// écriture exclusive
rwlock.lock();
defer rwlock.unlock();
```

### Semaphore

```zig
var sem = std.Thread.Semaphore{ .permits = 4 };   // 4 slots

sem.wait();        // acquire
defer sem.post();  // release
```

Équivalent direct du `sem = make(chan struct{}, runtime.NumCPU())` du projet Go.

### Condition

```zig
var mutex = std.Thread.Mutex{};
var cond  = std.Thread.Condition{};

// producteur
mutex.lock();
// modifier état partagé
cond.signal();     // réveiller un waiter (ou broadcast pour tous)
mutex.unlock();

// consommateur
mutex.lock();
while (!conditionRemplie()) {
    cond.wait(&mutex);   // relâche le mutex et attend
}
mutex.unlock();
```

---

## Thread Pool — `std.Thread.Pool`

```zig
var pool: std.Thread.Pool = undefined;
try pool.init(.{
    .allocator = allocator,
    .n_jobs    = 4,          // null = nombre de cœurs logiques
});
defer pool.deinit();

// Soumettre un job
try pool.spawn(maFonction, .{arg});
```

`pool.deinit()` attend la fin de tous les jobs en cours — équivalent de `wg.Wait()` en Go.

---

## Atomics — `std.atomic.Value(T)`

```zig
var counter = std.atomic.Value(i64).init(0);

// Incrémenter
_ = counter.fetchAdd(1, .seq_cst);

// Lire
const v = counter.load(.seq_cst);

// Écrire
counter.store(10, .seq_cst);

// Compare-and-swap
const ok = counter.cmpxchgStrong(10, 20, .seq_cst, .seq_cst);
```

### Memory orderings (explicites en Zig, cachés en Go)

| Ordering | Usage |
|---|---|
| `.seq_cst` | Ordre total — le plus sûr, le plus lent |
| `.acquire` | Lecture — garantit que les opérations suivantes ne remontent pas avant |
| `.release` | Écriture — garantit que les opérations précédentes ne descendent pas après |
| `.monotonic` | Pas de garantie d'ordre — compteurs purs, le plus rapide |

**Règle simple** : utiliser `.seq_cst` par défaut, optimiser avec `.acquire`/`.release` seulement si profilage le justifie.

---

## Async/await — suspendu depuis Zig 0.12

Zig avait un système async/await (coroutines stackless, proches des goroutines légères) permettant la concurrence coopérative sans threads OS. Il a été **retiré en 0.12** — la complexité d'intégration avec le compilateur était trop élevée.

Il reviendra dans une future version. En attendant : **threads OS uniquement**.

---

## Comparaison Go vs Zig

| | Go | Zig |
|---|---|---|
| Modèle threading | M:N (goroutines → threads OS) | 1:1 (thread OS direct) |
| Légèreté | Goroutine ~2 KB | Thread OS ~1–8 MB stack |
| Scheduler | Runtime Go | OS uniquement |
| Channels | Natifs (`chan`) | Pas de natif — à implémenter |
| Async | Goroutines (runtime) | Suspendu depuis 0.12 |
| Atomics | `sync/atomic` — types ergonomiques | `std.atomic.Value(T)` + orderings explicites |
| Race detector | `go run -race` intégré | Pas d'équivalent intégré |
| Mémoire | GC | Manuelle (allocator explicite) |
| CPU affinity | Via syscall + `LockOSThread` | Via syscall directement |

### Ce que Zig gagne vs Go

- **Pas de GC** : latence prévisible, pas de pauses stop-the-world.
- **Contrôle total** : taille de pile, orderings mémoire, placement des allocations.
- **Pas de surcoût runtime** : pas de scheduler, pas de GC background.

### Ce que Zig perd vs Go

- **Pas de goroutines** : un thread = beaucoup plus de mémoire → difficile de lancer 10 000 workers.
- **Pas de channels natifs** : la communication inter-threads est à construire.
- **Plus verbeux** : tout ce que Go fait implicitement (scheduler, memory ordering) est explicite.
