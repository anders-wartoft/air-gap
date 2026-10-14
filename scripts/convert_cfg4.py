import re

path = "src/downstream/configuration.go"
src = open(path).read().split("\n")

p = re.compile(r'^(\s*)Logger\.Fatalf\("Error parsing --(\w+): %s", (\w+)\)$')
out = []
n = 0
for line in src:
    m = p.match(line)
    if m:
        indent, setting, var = m.groups()
        out.append(
            f'{indent}Logger.Fatalf(configInvalid("{setting}", {var}), "Error parsing --{setting}: %s", {var})'
        )
        n += 1
        continue
    out.append(line)
open(path, "w").write("\n".join(out))
print(n)
