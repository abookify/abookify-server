"""Book-agnostic cast extraction from BookNLP output.

Ports the LOCAL, name-driven stages of the validated prototype
(/home/pj/booknlp-prototype/pipeline2.py): normalize proper-name variants,
drop NER junk, pick the cleanest display name, rank by mention count. The
Frankenstein-specific first-person frame-split and the LLM-reduce stage are
intentionally omitted for the MVP (see SESSION_HANDOFF "Cast of characters"):
the result is an honest named cast that may over-split aliases sharing no
tokens, which is why every UI surface labels the feature EXPERIMENTAL.
"""
import json
import re
from collections import defaultdict

# Honorifics / descriptors stripped before comparing name tokens, so
# "Mr. Frankenstein" and "Frankenstein" compare equal and "poor Justine"
# reduces to "justine".
STRIP = set(
    "m mr mrs ms mme madame miss dr st sir lord lady dear dearest poor sweet good "
    "excellent loved enraged beloved little darling my our the a an old young his her "
    "their your blessed gentle noble kind lovely great presently very most more".split()
)
# Bare single-token clusters that BookNLP's NER mislabels as PROPER but are
# generic — dropped when they're the whole name.
JUNK = set(
    "begone alas adieu farewell god heaven gutenberg project nay oh ah ay yes no man woman "
    "child sir madam lord devil thing nothing day night time world life death heart soul "
    "father mother".split()
)

MIN_COUNT = 10  # ignore clusters mentioned fewer than this many times


def _toks(name):
    n = name.lower().split(",")[0]
    n = re.sub(r"[^a-z ]", " ", n)
    return [t for t in n.split() if t and t not in STRIP and len(t) > 1]


# A real character surface form is short. BookNLP's coref occasionally captures
# a whole clause as a "proper" mention (e.g. "Elizabeth, who agreed in wishing,
# for the sake of their sister's feelings…"); reject those so they can't
# pollute aliases or, worse, win the display name (which favors token count).
def _namelike(s):
    s = s.strip()
    return len(s) <= 40 and len(s.split()) <= 5



def extract_cast(book_path):
    """Return a ranked list of characters from a BookNLP .book JSON file.

    Each entry: {name, aliases, gender, mention_count}. Only proper-named
    characters survive (the conflated first-person narrator blob and
    common-noun-only fragments are left out of the MVP cast).
    """
    book = json.load(open(book_path))

    # Collect proper-named clusters above the count floor.
    C = {}
    for c in book.get("characters", []):
        if c.get("count", 0) < MIN_COUNT:
            continue
        m = c.get("mentions", {})
        proper = [x["n"] for x in m.get("proper", []) if _namelike(x["n"])]
        if not proper:
            continue  # MVP: named characters only
        C[c["id"]] = {
            "count": c["count"],
            "proper": proper,
            "gender": (c.get("g", {}) or {}).get("argmax", "") or "",
        }

    # Per-cluster token set + cleanest display name (most content tokens,
    # fewest words). Drop clusters that are entirely junk/honorifics.
    atok, bestname = {}, {}
    for i, v in C.items():
        cand = sorted(v["proper"], key=lambda s: (-len(_toks(s)), len(s.split())))
        nm = cand[0] if cand else ""
        ts = set(_toks(nm))
        if not ts or all(t in JUNK for t in ts):
            continue
        atok[i] = ts
        bestname[i] = nm.split(",")[0].strip()

    # Union-find merge of proper-name variants. A cluster folds into another
    # only when its tokens are a strict subset of EXACTLY ONE other cluster
    # (the unique-superset guard keeps "William Frankenstein" and "Ernest
    # Frankenstein" apart), or into a same-token cluster with >= count.
    parent = {i: i for i in atok}

    def root(x):
        while parent[x] != x:
            parent[x] = parent[parent[x]]
            x = parent[x]
        return x

    for a in list(atok):
        sup = [b for b in atok if b != a and atok[a] < atok[b]]
        if len(sup) == 1:
            parent[root(a)] = root(sup[0])
        for b in [b for b in atok if b != a and atok[b] == atok[a] and C[b]["count"] >= C[a]["count"]]:
            parent[root(a)] = root(b)

    grp = defaultdict(list)
    for i in atok:
        grp[root(i)].append(i)

    cast = []
    for r, mem in grp.items():
        mem.sort(key=lambda i: -C[i]["count"])
        # Display name: fewest stop-words, then shortest.
        name = min(
            (bestname[i] for i in mem),
            key=lambda s: (len([w for w in s.split() if w.lower() in STRIP]), len(s)),
        )
        aliases = sorted({a for i in mem for a in C[i]["proper"]})
        # Gender: take the highest-count member's argmax.
        gender = C[mem[0]]["gender"]
        cast.append({
            "name": name,
            "aliases": aliases,
            "gender": gender,
            "mention_count": sum(C[i]["count"] for i in mem),
        })

    cast.sort(key=lambda c: -c["mention_count"])
    return cast
