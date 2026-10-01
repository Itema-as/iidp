package apprepo

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/Itema-as/iidp/internal/platformrepo"
)

// NonRootFix is how Adopt makes a repository's own Dockerfile run as a
// non-root user: the chart sets runAsNonRoot, and the kubelet can check
// that only against a numeric USER. Adopt changes the Dockerfile only where
// the fix is known, and otherwise says what to change.
type NonRootFix struct {
	// Dockerfile is the changed Dockerfile, nil when Adopt changes nothing.
	Dockerfile []byte
	// Change says what was changed and why, for the pull request body.
	Change string
	// Advice says what to change by hand, when the image may run as root
	// and Adopt can't tell how to fix it. Empty when the image runs as a
	// numeric non-root user, or when Change fixes it.
	Advice string
}

// The users and ports the images Adopt knows run with: the official node
// image's node user, and the unprivileged nginx, which runs as 101 and
// listens on 8080 where the official nginx runs as root on 80.
const (
	nodeUser          = "1000:1000"
	unprivilegedNginx = "nginxinc/nginx-unprivileged"
)

// fixNonRoot reads dockerfile's final stage, the one the image is built
// from, and decides who it runs as: the last USER in that stage or in the
// stages it is built FROM, else the base image's own user. A numeric user
// other than 0 needs nothing. The known fixes:
//   - a named user whose number the Dockerfile itself shows, with a
//     useradd or adduser -u/--uid that creates it: that number;
//   - a Node base image with no USER, or USER root or 0: USER 1000:1000,
//     the node user, added at the end of the stage; USER node becomes the
//     same number;
//   - an nginx base image, for a Static site: nginxinc/nginx-unprivileged
//     at the same tag, which listens on 8080, as long as the stage runs no
//     command and ships no nginx config of its own that the non-root
//     nginx could trip on.
//
// Anything else that may run as root gets Advice instead.
func fixNonRoot(dockerfile []byte, kind string) NonRootFix {
	df := parseDockerfile(string(dockerfile))
	if len(df.stages) == 0 {
		return NonRootFix{Advice: "iidp found no FROM in the Dockerfile. " + adviceNumericUser}
	}
	chain := df.chain(len(df.stages) - 1)
	base := chain[len(chain)-1].image
	for _, s := range chain {
		if at := s.last("USER"); at >= 0 {
			return fixUser(df, chain, s.instructions[at], base, kind)
		}
	}
	if base.is(unprivilegedNginx) {
		return NonRootFix{}
	}
	return fixRoot(df, chain, "", base, kind)
}

// adviceNumericUser is what every piece of Advice ends with.
const adviceNumericUser = "The Platform starts a container only as a non-root user it can check, which means a numeric `USER` other than 0 in the final stage, such as `USER 1000:1000`, and an application that runs as that user."

// fixUser handles a final stage whose user is set by USER instruction in,
// the last one in the stage or the nearest stage it is built FROM.
func fixUser(df dockerfile, chain []stage, in instruction, base image, kind string) NonRootFix {
	user := in.args
	name, group, _ := strings.Cut(user, ":")
	if strings.Contains(name, "$") {
		return NonRootFix{Advice: fmt.Sprintf("The final stage runs as `USER %s`, which iidp can't resolve. %s", user, adviceNumericUser)}
	}
	if uid, err := strconv.Atoi(name); err == nil {
		if uid != 0 {
			return NonRootFix{}
		}
		return fixRoot(df, chain, "USER "+user, base, kind)
	}
	if name == "root" {
		return fixRoot(df, chain, "USER "+user, base, kind)
	}
	if uid, how, ok := createdUID(chain, name); ok {
		numeric := uid
		if group != "" {
			numeric += ":" + group
		}
		return NonRootFix{
			Dockerfile: df.replace(in, "USER "+numeric),
			Change:     fmt.Sprintf("`USER %s` becomes `USER %s`, the number the Dockerfile gives it (`%s`): the Platform can check only a numeric user is not root.", user, numeric, how),
		}
	}
	if base.is("node") && name == "node" && (group == "" || group == "node") {
		return NonRootFix{
			Dockerfile: df.replace(in, "USER "+nodeUser),
			Change:     fmt.Sprintf("`USER %s` becomes `USER %s`, the node image's own `node` user by number: the Platform can check only a numeric user is not root.", user, nodeUser),
		}
	}
	return NonRootFix{Advice: fmt.Sprintf("The final stage runs as `USER %s`, and the Dockerfile doesn't show that user's number. Change it to the number the image gives %s. %s", user, name, adviceNumericUser)}
}

// fixRoot handles a final stage that runs as root: said so by root, a USER
// instruction, or, when root is "", by having no USER on a base image that
// runs as root or might.
func fixRoot(df dockerfile, chain []stage, root string, base image, kind string) NonRootFix {
	final := chain[0]
	switch {
	case base.is("node"):
		why := "has no `USER`, so it runs as root"
		if root != "" {
			why = fmt.Sprintf("runs as root (`%s`)", root)
		}
		return NonRootFix{
			Dockerfile: df.insertUser(final, nodeUser),
			Change:     fmt.Sprintf("The final stage %s. It now ends with `USER %s`, the node image's own `node` user, by number. Anything the application writes while it runs must be writable by that user.", why, nodeUser),
		}
	case base.is("nginx"):
		if reason := nginxUnfixable(chain, root, base, kind); reason != "" {
			return NonRootFix{Advice: fmt.Sprintf("The final stage is nginx (`%s`), which runs as root on port 80. The Platform runs every container as non-root, and serves a Static site on 8080: switch to `%s` at the same tag, and make sure nginx listens on 8080, which its default config does. iidp didn't change it because %s.", base.ref, unprivilegedNginx, reason)}
		}
		return fixNginx(df, chain, base)
	}
	if root != "" {
		return NonRootFix{Advice: fmt.Sprintf("The final stage runs as root (`%s`). %s", root, adviceNumericUser)}
	}
	return NonRootFix{Advice: fmt.Sprintf("The final stage has no `USER`, and iidp can't tell who its base image (`%s`) runs as. %s", base.ref, adviceNumericUser)}
}

// nginxUnfixable says why an nginx stage can't simply be moved to the
// unprivileged nginx, or "" when it can. The unprivileged image sets USER
// 101, so every RUN after FROM would run as 101 too, and its nginx.conf
// keeps its pid and temporary files under /tmp, which a config shipped
// over it would not.
func nginxUnfixable(chain []stage, root string, base image, kind string) string {
	switch {
	case kind != platformrepo.KindStaticSite:
		return "a Web service listens on its Environment's `port`, which iidp would have to change with it"
	case root != "":
		return fmt.Sprintf("the Dockerfile asks for root (`%s`)", root)
	case base.digest:
		return "the image is pinned by digest, which the unprivileged image doesn't share"
	case !base.literal:
		return "its name comes from a build argument"
	}
	for _, s := range chain {
		for _, in := range s.instructions {
			switch {
			case in.cmd == "RUN":
				return "the stage runs commands (`RUN`), which the unprivileged image runs as its non-root user"
			case (in.cmd == "COPY" || in.cmd == "ADD") && strings.Contains(in.args, "/etc/nginx"):
				return "it ships nginx configuration of its own, which must listen on 8080 and keep its pid and temporary files where a non-root user can write"
			}
		}
	}
	return ""
}

// fixNginx moves the stage built FROM nginx to the unprivileged nginx at
// the same tag, and EXPOSE 80 to 8080.
func fixNginx(df dockerfile, chain []stage, base image) NonRootFix {
	from := chain[len(chain)-1].from
	lines := df.replaceText(from, base.name, unprivilegedNginx)
	change := fmt.Sprintf("`FROM %s` becomes `FROM %s`, the same nginx running as a non-root user (101), which listens on 8080, where the Platform serves a Static site.", base.ref, strings.Replace(base.ref, base.name, unprivilegedNginx, 1))
	for _, s := range chain {
		for _, in := range s.instructions {
			if in.cmd == "EXPOSE" && slices.ContainsFunc(strings.Fields(in.args), func(p string) bool { return p == "80" || p == "80/tcp" }) {
				lines = dockerfile{lines: lines}.replaceText(in, "80", "8080")
				change += " `EXPOSE 80` becomes `EXPOSE 8080` with it."
			}
		}
	}
	return NonRootFix{Dockerfile: []byte(strings.Join(lines, "\n") + "\n"), Change: change}
}

// createdUID finds the useradd or adduser in chain's RUN instructions that
// creates name with an explicit -u or --uid, and returns that number and
// the command that set it. Two different numbers for the same name are
// not a known fix.
func createdUID(chain []stage, name string) (uid, how string, ok bool) {
	for _, s := range chain {
		for _, in := range s.instructions {
			if in.cmd != "RUN" {
				continue
			}
			for _, command := range shellCommands.Split(in.args, -1) {
				words := strings.Fields(command)
				if len(words) < 2 || words[len(words)-1] != name {
					continue
				}
				program := words[0][strings.LastIndex(words[0], "/")+1:]
				if program != "useradd" && program != "adduser" {
					continue
				}
				for i, w := range words[1 : len(words)-1] {
					var value string
					switch {
					case w == "-u" || w == "--uid":
						value = words[i+2]
					case strings.HasPrefix(w, "--uid="):
						value = strings.TrimPrefix(w, "--uid=")
					default:
						continue
					}
					if _, err := strconv.Atoi(value); err != nil {
						return "", "", false
					}
					if ok && value != uid {
						return "", "", false
					}
					uid, how, ok = value, program+" "+w+" "+value, true
					if strings.HasPrefix(w, "--uid=") {
						how = program + " " + w
					}
				}
			}
		}
	}
	return uid, how, ok
}

// shellCommands splits a RUN line into its commands.
var shellCommands = regexp.MustCompile(`&&|\|\||;|\|`)

// dockerfile is a Dockerfile split into its lines, and the stages its
// instructions make up.
type dockerfile struct {
	lines  []string
	stages []stage
}

// stage is one FROM and the instructions after it.
type stage struct {
	from         instruction
	image        image
	name         string
	instructions []instruction
	// parent is the index of the stage this one is built FROM, or -1
	// when it is built from an image.
	parent int
}

// instruction is one Dockerfile instruction: its upper-cased command, its
// arguments with continuation lines joined, and the lines it spans.
type instruction struct {
	cmd         string
	args        string
	first, last int
	// above is the first line of the comment block directly above it, or
	// first when there is none.
	above int
}

// image is a base image reference as a FROM names it.
type image struct {
	// ref is the reference as written, build arguments resolved.
	ref string
	// name is the repository as written: nginx, docker.io/library/nginx.
	name string
	// repository is name without Docker Hub's default registry and
	// library/ prefix.
	repository string
	digest     bool
	// literal is false when the repository came from a build argument.
	literal bool
}

func (i image) is(repository string) bool { return i.repository == repository }

// last returns the index of the last instruction cmd in s, or -1.
func (s stage) last(cmd string) int {
	for i := len(s.instructions) - 1; i >= 0; i-- {
		if s.instructions[i].cmd == cmd {
			return i
		}
	}
	return -1
}

// chain returns stage i and the stages it is built FROM, in that order:
// the last one is built from an image.
func (d dockerfile) chain(i int) []stage {
	var out []stage
	for ; i >= 0; i = d.stages[i].parent {
		out = append(out, d.stages[i])
	}
	return out
}

// heredoc matches the start of a BuildKit here-document, <<EOF or <<-"EOF".
var heredoc = regexp.MustCompile(`<<-?["']?([A-Za-z_][A-Za-z0-9_]*)["']?`)

// parseDockerfile splits content into instructions and stages. Global ARG
// defaults (before the first FROM) are substituted into FROM lines, which
// is how the node templates name their image (node:${NODE_VERSION}).
func parseDockerfile(content string) dockerfile {
	d := dockerfile{lines: strings.Split(strings.TrimSuffix(content, "\n"), "\n")}
	args := map[string]string{}
	names := map[string]int{}
	comment := -1
	for i := 0; i < len(d.lines); i++ {
		line := strings.TrimSpace(d.lines[i])
		switch {
		case line == "":
			comment = -1
			continue
		case strings.HasPrefix(line, "#"):
			if comment < 0 {
				comment = i
			}
			continue
		}
		in := instruction{first: i, above: i}
		if comment >= 0 {
			in.above = comment
		}
		comment = -1
		text := line
		for strings.HasSuffix(text, `\`) && i+1 < len(d.lines) {
			i++
			next := strings.TrimSpace(d.lines[i])
			if strings.HasPrefix(next, "#") {
				continue
			}
			text = strings.TrimSuffix(text, `\`) + " " + next
		}
		if m := heredoc.FindStringSubmatch(text); m != nil {
			for i+1 < len(d.lines) {
				i++
				if strings.TrimSpace(d.lines[i]) == m[1] {
					break
				}
			}
		}
		in.last = i
		cmd, rest := text, ""
		if space := strings.IndexAny(text, " \t"); space >= 0 {
			cmd, rest = text[:space], text[space+1:]
		}
		in.cmd, in.args = strings.ToUpper(cmd), strings.TrimSpace(rest)

		switch {
		case in.cmd == "ARG" && len(d.stages) == 0:
			if name, value, ok := strings.Cut(in.args, "="); ok {
				args[name] = strings.Trim(value, `"'`)
			}
		case in.cmd == "FROM":
			s := stage{from: in, parent: -1}
			words := slices.DeleteFunc(strings.Fields(in.args), func(w string) bool { return strings.HasPrefix(w, "--") })
			if len(words) == 0 {
				continue
			}
			if len(words) >= 3 && strings.EqualFold(words[1], "AS") {
				s.name = strings.ToLower(words[2])
			}
			if p, ok := names[strings.ToLower(words[0])]; ok {
				s.parent = p
			} else {
				s.image = parseImage(words[0], args)
			}
			if s.name != "" {
				names[s.name] = len(d.stages)
			}
			d.stages = append(d.stages, s)
		case len(d.stages) > 0:
			d.stages[len(d.stages)-1].instructions = append(d.stages[len(d.stages)-1].instructions, in)
		}
	}
	return d
}

// argRef matches a build argument in a FROM line: $NAME or ${NAME}.
var argRef = regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?`)

// parseImage reads a FROM image reference, substituting args.
func parseImage(ref string, args map[string]string) image {
	resolved := argRef.ReplaceAllStringFunc(ref, func(m string) string {
		if v, ok := args[argRef.FindStringSubmatch(m)[1]]; ok {
			return v
		}
		return m
	})
	img := image{ref: resolved}
	name := resolved
	if at := strings.Index(name, "@"); at >= 0 {
		name, img.digest = name[:at], true
	}
	if colon := strings.LastIndex(name, ":"); colon > strings.LastIndex(name, "/") {
		name = name[:colon]
	}
	img.name = name
	img.literal = strings.Contains(ref, name)
	repository := strings.ToLower(name)
	for _, prefix := range []string{"docker.io/", "index.docker.io/", "registry-1.docker.io/"} {
		repository = strings.TrimPrefix(repository, prefix)
	}
	img.repository = strings.TrimPrefix(repository, "library/")
	return img
}

// replace returns the Dockerfile with in's lines replaced by one line,
// text, indented as in's first line was.
func (d dockerfile) replace(in instruction, text string) []byte {
	indent := d.lines[in.first][:len(d.lines[in.first])-len(strings.TrimLeft(d.lines[in.first], " \t"))]
	lines := slices.Concat(d.lines[:in.first], []string{indent + text}, d.lines[in.last+1:])
	return []byte(strings.Join(lines, "\n") + "\n")
}

// replaceText returns the Dockerfile's lines with the first whole-word
// old in in's lines replaced by new.
func (d dockerfile) replaceText(in instruction, old, new string) []string {
	lines := slices.Clone(d.lines)
	word := regexp.MustCompile(`(^|[\s=])` + regexp.QuoteMeta(old) + `([\s:@/]|$)`)
	for i := in.first; i <= in.last; i++ {
		if loc := word.FindStringSubmatchIndex(lines[i]); loc != nil {
			lines[i] = lines[i][:loc[3]] + new + lines[i][loc[4]:]
			break
		}
	}
	return lines
}

// insertUser returns the Dockerfile with USER user added to the final
// stage s after everything that runs or writes as the build's user:
// before the instructions that only describe the image (CMD, ENTRYPOINT,
// EXPOSE and the like) it ends with, and the comments above them.
func (d dockerfile) insertUser(s stage, user string) []byte {
	at := len(d.lines)
	for i := len(s.instructions) - 1; i >= 0; i-- {
		if !slices.Contains([]string{"CMD", "ENTRYPOINT", "EXPOSE", "HEALTHCHECK", "LABEL", "STOPSIGNAL"}, s.instructions[i].cmd) {
			break
		}
		at = s.instructions[i].above
	}
	added := []string{"# Non-root, by number: the Platform starts no container as root.", "USER " + user}
	if at > 0 && strings.TrimSpace(d.lines[at-1]) != "" {
		added = append([]string{""}, added...)
	}
	if at < len(d.lines) {
		added = append(added, "")
	}
	lines := slices.Concat(d.lines[:at], added, d.lines[at:])
	return []byte(strings.Join(lines, "\n") + "\n")
}
