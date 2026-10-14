import re

path = "src/downstream/configuration.go"
src = open(path).read().split("\n")

ovr = re.compile(
    r'^(\s*)Logger\.Print\("Overriding (\w+) with environment variable: " \+ prefix \+ "(\w+)" \+ " with value: " \+ (\w+)\)$'
)
fatal = re.compile(
    r'^(\s*)Logger\.Fatalf\("Error in config (\w+)\. Illegal value: %s\.(.*?)", (\w+)\)$'
)

out = []
nov = nfa = 0
for line in src:
    m = ovr.match(line)
    if m:
        indent, setting, env, var = m.groups()
        out.append(
            f'{indent}Logger.Info(configStr(logging.EventConfigOverride, "{setting}", {var}).With(\n'
            f'{indent}\tlogging.Str("airgap.config.source", prefix+"{env}"),\n'
            f'{indent}), "Overriding {setting} with environment variable: "+prefix+"{env}"+" with value: "+{var})'
        )
        nov += 1
        continue
    m = fatal.match(line)
    if m:
        indent, setting, tail, var = m.groups()
        out.append(
            f'{indent}Logger.Fatalf(configInvalid("{setting}", {var}), '
            f'"Error in config {setting}. Illegal value: %s.{tail}", {var})'
        )
        nfa += 1
        continue
    out.append(line)

open(path, "w").write("\n".join(out))
print("overrides", nov, "fatals", nfa)
