import re

path = "src/downstream/configuration.go"
src = open(path).read().split("\n")

pat = re.compile(r'^(\s*)Logger\.Printf\("( *)(\S+?): %([sdt])", (.+)\)$')

out = []
in_log_conf = False
for line in src:
    if line.startswith("func logConfiguration("):
        in_log_conf = True
    elif line.startswith("func ") and in_log_conf:
        in_log_conf = False
    m = pat.match(line)
    if not m:
        out.append(line)
        continue
    indent, pad, setting, verb, arg = m.groups()
    event = "logging.EventConfigApplied" if in_log_conf else "logging.EventConfigParse"
    if verb == "s":
        helper = f'configStr({event}, "{setting}", {arg})'
    elif verb == "d":
        helper = f'configInt({event}, "{setting}", int64({arg}))'
    else:
        helper = f'configBool({event}, "{setting}", {arg})'
    out.append(f'{indent}Logger.Infof({helper}, "{pad}{setting}: %{verb}", {arg})')

open(path, "w").write("\n".join(out))
print("done")
