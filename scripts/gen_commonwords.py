#!/usr/bin/env python3
"""Generate a Go source file embedding a modest common-English-word list.

The list is a hand-assembled set of common English words plus mechanical
inflections of common lemmas. Individual common words are facts, and a plain
sorted list of them carries no creative expression, so it is license-safe (no
external corpus was copied). It is used only to filter ordinary English words out
of the rare-repeated-lowercase proper-noun-candidate heuristic.
"""

# Base lemmas / common words. Hand-authored. Kept lowercase.
BASE = """
a about above across act add afraid after afternoon again against age ago agree
ah air all allow almost alone along already alright also although always am among
amount an and anger angry animal another answer any anyone anything anyway apart
appear apple April are area arm army around arrive art as ask asleep at ate
attack attention August aunt autumn away baby back bad bag ball band bank bar bare
base bath be beach bear beat beautiful beauty became because become bed been
before began begin behind being believe bell belong below belt bench beneath
beside best better between beyond big bird birth bit bite black blade blank blaze
bleed blind block blood blow blue board boat body bone book boot border born
both bother bottle bottom bought bound bowl box boy branch brave bread break
breakfast breath breathe bridge bright bring broad broke broken brother brought
brown brush build building built burn burst business busy but butter button buy
by cabin cage call calm came camp can candle cannot cap captain car card care
careful carry case cast castle cat catch caught cause cave cease cell center
century certain chain chair chance change chapter character charge chase cheap
cheek cheer cheese chest chief child children chin choice choose chose chosen
church circle city claim class claw clay clean clear clever cliff climb cloak
clock close cloth clothes cloud club coast coat cold collar collect color come
comfort coming command common company complete concern condition confuse
consider contain continue control cook cool copper copy corner corps corpse cost
cottage cotton could council count country couple courage course court cousin
cover cow crack cradle craft crash crawl crazy cream create creature creep cried
crime cross crowd crown cruel crush cry cup cure curious current curse curtain
curve custom cut daily damage damp dance danger dare dark darkness date daughter
dawn day dead deaf deal dear death December decide decision deck deep deeply deer
defeat defend degree delay delight deliver demand den depend des describe desert
desire desk despair destroy detail develop device devil did die difference
different difficult dig dinner direct direction dirt dirty distance distant divide
do doctor does dog dollar done door double doubt down dozen drag dragon draw dread
dream dress drew dried drink drive driven drop drove drown drug drum drunk dry
duck due dull during dust duty each ear early earn earth ease east easy eat edge
effect effort egg eight either else empty end enemy enough enter entire equal
escape especially even evening event ever every everyone everything everywhere evil
exact example except exchange excited excitement excuse exist expect experience
explain express extra eye face fact fade fail faint fair faith fall fallen false
family famous far farm farther fashion fast fasten fat fate father fault favor
fear feast feather February feed feel feeling feet fell fellow felt fence fever
few field fierce fight figure fill final find fine finger finish fire firm first
fish fist fit five fix flag flame flash flat fled flee flesh flew flight float
floor flow flower flung fly fog fold folk follow food fool foot for force
forehead foreign forest forever forget forgive forgot form former fort forth
fortune forward fought found four frame free fresh Friday friend friendship
fright frighten from front frost frown froze fruit full fun funny fur furnish
further gain game garden gate gather gave gay gaze general gentle gentleman
gently get ghost giant gift girl give given glad glance glass gleam glimpse glory
glow go goal god gold golden gone good goodbye got govern grab grace grade grain
grand grandfather grandmother grant grasp grass grateful grave gray great green
greet grew grey grief grin grind grip groan ground group grove grow grown growth
guard guess guest guide gun guy had hair half hall halt hand handle handsome hang
happen happy harbor hard hardly harm harvest has haste hasten hat hate have he
head heal health heap hear heard heart heat heaven heavy heel held hell hello
help her herd here hero herself hidden hide high hill him himself hint hip hire
his hit hold hole hollow holy home honest honor hook hope horn horror horse hot
hotel hour house hover how however huge human humble hundred hung hunger hungry
hunt hurried hurry hurt husband hut ice idea if ill imagine immediately important
in inch indeed indoor influence inform inn inner inside instant instead interest
into iron is island it its itself January jaw job join joint joke journey joy
judge July jump June just keen keep kept key kick kill kind king kingdom kiss
kitchen knee kneel knee knew knife knight knit knock knot know knowledge known
labor lack lad ladder lady laid lake lamp land lane language lantern lap large
last late later laugh law lay lead leader leaf lean leap learn least leather leave
led left leg lend length less lesson let letter level lie life lift light like
likely limb limit line linger lion lip liquid list listen little live load loaf
local lock log lonely long look loose lord lose loss lost lot loud love low lower
loyal luck lucky lump lunch lung mad made magic maid mail main major make male man
manage manner many map march mark market marriage marry mask master mat match
mate matter may maybe me meal mean meaning meant meanwhile measure meat meet
melt member memory men mend mention mercy mere merely merry message met metal
middle might mighty mild mile milk mill mind mine minute mirror miss mist mistake
mistress mix moment money monkey month mood moon moral more morning most mother
motion mount mountain mouse mouth move movement much mud murder muscle music must
my myself nail naked name narrow nation native nature near nearly neat neck need
needle neighbor neither nephew nerve nervous nest net never new news next nice
niece night nine no nobody nod noise none noon nor normal north nose not note
nothing notice November now nowhere number nurse nut oak oath obey object observe
ocean o'clock October of off offer office officer often oh oil old on once one
only onto open or order ordinary other otherwise ought our ourselves out outside
over owe own owner ox pace pack page pain paint pair pale palm pan pale panel
paper parcel pardon parent park part particular partner party pass passage past
path patient pause paw pay peace pen pencil people perfect perhaps period perish
permit person pet phone pick picture piece pig pile pillow pin pine pink pipe
pity place plain plan plant plate play pleasant please pleasure plenty plow pocket
poem point poison pole police polish polite pool poor pope pop popular port
portion position possible post pot pound pour poverty powder power practice praise
pray prayer preach precious prepare presence present preserve press pretend
pretty prevent price pride priest prince princess print prison private prize
probably problem produce promise proper property protect proud prove provide
public pull punish pupil pure purple purpose purse push put quarter queen queer
question quick quiet quit quite race rag rail rain raise ran rang range rank rap
rapid rare rat rate rather raw ray reach read ready real realize really reason
receive recent recognize record red refuse regard region regret regular reign
reject rejoice remain remember remind remove rent repair repeat reply report rest
result return reveal reward rich rid ride ridge right ring rise risk river road
roar rob robe rock rod roll roof room root rope rose rough round row royal rub
rude rug ruin rule run rush rust sacred sacrifice sad saddle safe safety said sail
sailor saint sake salt same sand sang sat satisfy Saturday save saw say scale
scarce scarcely scatter scene scent school science scorn scrape scratch scream
sea search season seat second secret secure see seed seek seem seen seize seldom
self sell send sense sent sentence separate September serious servant serve
service set settle seven several severe sew shade shadow shake shall shame shape
share sharp she shed sheep sheet shelf shell shelter shepherd shield shine ship
shirt shock shoe shone shook shoot shop shore short shot should shoulder shout
show shower shut sick side sigh sight sign signal silence silent silk silver
similar simple simply sin since sing single sink sir sister sit situation six
size skill skin skirt sky slave sleep sleeve slept slide slight slip slope slow
small smart smell smile smoke smooth snake snap snow so social society soft soil
soldier sole solemn solid some somebody somehow someone something sometimes
somewhat somewhere son song soon sore sorrow sorry sort soul sound soup source
south space spade spare speak spear special speech speed spell spend spent spider
spirit spite spoke spoken spot spread spring square staff stage stair stand star
stare start state station stay steady steal steam steel steep steer stem step
stern stick stiff still sting stir stock stomach stone stood stool stop store
storm story stove straight strange stranger straw stream street strength stretch
strike string strip stroke strong struck struggle student study stuff stumble
stupid subject succeed success such sudden suddenly suffer sugar suggest suit
summer sun Sunday sung sunk sunlight sunny sunset sunshine supper suppose sure
surely surface surprise surround suspect swallow swam swear sweat sweep sweet
swell swept swift swim swing sword swore sworn table tail take taken tale talk
tall tap task taste taught tax tea teach teacher team tear teeth tell temper
temple ten tend tender term terms terrible terror test than thank that the their
them themselves then there therefore these they thick thief thin thing think
third thirst thirty this thorn those though thought thousand thread threat three
threw throat throne through throughout throw thrown thrust thumb thunder thus tie
tight till time tin tiny tip tire tired title to today toe together told tomorrow
tone tongue tonight too took tool tooth top tore torn total touch toward tower
town toy trace track trade trail train trap travel treasure treat tree tremble
trial tribe trick tried trip troop trouble true trust truth try tube Tuesday tune
turn twelve twenty twice twist two type ugly uncle under understand understood
uneasy unhappy uniform union unit unite universe unless until unto up upon upper
upright upset upstairs upward urge us use used useful useless usual usually valley
value various vast vein very vessel victory view village voice vote wage wagon
waist wait wake walk wall wander want war warm warn warrior was wash waste watch
water wave way we weak weakness wealth weapon wear weary weather weave web wed
Wednesday wee week weep weigh weight welcome well went were west wet what whatever
wheat wheel when whenever where wherever whether which while whip whisper whistle
white who whole whom whose why wicked wide widow wife wild will willing win wind
window wine wing wink winter wipe wire wise wish wit witch with within without
witness woke wolf woman women won wonder wonderful wood wooden wool word wore work
world worm worn worry worse worship worst worth would wound wove wrap wrist write
writer written wrong wrote yard yawn year yellow yes yesterday yet yield you young
your yours yourself youth
""".split()

# Common lemmas we expand with regular inflections to broaden coverage cheaply.
INFLECT = """
act add agree allow answer appear arrive ask attack bake bark beg blend bless
block blow board boil bolt bomb book boot border borrow bounce bow brush build
burn bury call calm camp care carry cause chain chase check cheer chew claim
clean clear climb close cloud collect color comb command consider contain
continue control cook cool copy count cover crack crawl cross crowd crush cry cure
curl curse dance dare deal decide defend deliver demand depend describe deserve
desire destroy develop die dig direct discover divide draw dream dress drift drop
drown dry earn eat enter escape examine expect explain explore express face fade
fail fall fasten favor fear feed feel fetch fill finish fix flash float flood flow
fold follow fool force form frame frighten frown gain gather gaze glance glow grab
grant grasp greet grin grip groan grow guard guess guide hammer handle hang harm
hate heal heap hear heat help hide hint hire hope hover hug hum hunt hurry imagine
join joke judge jump keep kick kill kiss kneel knit knock knot laugh lead lean
leap learn leave lend lift limit link listen live load lock long look loosen love
lower mark marry match matter melt mend mention mix move murmur nail need nod note
notice number obey observe offer open order pack paint park pass pause pick pile
pin plant play please plow point poison polish pour praise pray preach prepare
present preserve press pretend prevent print promise protect prove provide pull
punish push race rain raise reach read realize receive recognize record refuse
regard regret reject remain remember remind remove rent repair repeat reply report
rescue rest return reveal reward ring rise roar roll rub ruin rule rush save scan
scatter scent scoop scorn scrape scratch scream search seek seem seize sell send
serve settle shake shape share shatter shine shiver shock shoot shout shove show
shower shrug shut sigh sign sing sink sip slam slap sleep slide slip smash smell
smile smoke snap sneak sniff snore soak sob soften solve sort speak spell spend
spill spin splash split spoil spot spray spread sprinkle spy squeeze stain stand
stare start starve stay steal steer step stick sting stir stitch stop store storm
stretch strike stroke struggle study stumble suck suffer suggest supply support
suppose surround suspect swallow swear sweat sweep swell swim swing talk tap taste
teach tear tease tell tend test thank thaw think thread threaten throw thrust tie
tip touch trace track trade trail train trap travel treat tremble trick trip trot
trust try tuck tug turn twist type urge use vanish veil vote wag wait wake walk
wander want warm warn wash waste watch wave weep welcome whip whisper whistle
wipe wish wonder work worry worship wound wrap yawn yell yield
""".split()


def inflect(w):
    forms = {w}
    forms.add(w + "s")
    forms.add(w + "ing")
    forms.add(w + "ed")
    forms.add(w + "er")
    if w.endswith("e"):
        forms.add(w[:-1] + "ing")
        forms.add(w + "d")
        forms.add(w + "r")
    if w.endswith(("s", "x", "z", "ch", "sh")):
        forms.add(w + "es")
    if w.endswith("y") and len(w) > 1 and w[-2] not in "aeiou":
        forms.add(w[:-1] + "ies")
        forms.add(w[:-1] + "ied")
    return forms


# Adjectives we expand into -ly adverbs and comparative/superlative forms.
ADJ = """
quick slow clear bright dark deep high low near far wide short tall broad thin
thick soft hard loud quiet calm gentle rough smooth sharp dull warm cold hot cool
strong weak brave bold fierce proud humble kind cruel wise foolish clever smart
dull rich poor happy sad angry glad calm eager weary tired fresh clean dirty pure
plain fancy simple complex easy hard heavy light dense loose tight full empty open
closed final actual usual normal typical special general common rare frequent
recent current constant sudden slight great small large tiny huge vast wide narrow
early late quick brief long short direct indirect certain sure clear obvious
apparent evident likely main chief major minor basic total complete partial
perfect exact precise rough true false real proper correct wrong right careful
careless quiet loud swift steady firm gradual immediate instant final absolute
mere sheer utter total complete
""".split()

words = set()
for w in BASE:
    for f in inflect(w.lower()):
        words.add(f)
for w in INFLECT:
    for f in inflect(w.lower()):
        words.add(f)
for w in ADJ:
    w = w.lower()
    words.add(w)
    words.add(w + "ly")
    words.add(w + "er")
    words.add(w + "est")
    if w.endswith("y") and len(w) > 1 and w[-2] not in "aeiou":
        words.add(w[:-1] + "ily")
        words.add(w[:-1] + "ier")
        words.add(w[:-1] + "iest")
    if w.endswith("e"):
        words.add(w[:-1] + "y")

# Supplemental common vocabulary that a base+inflection pass misses: abstract nouns,
# modern/everyday words, adverbs, and the recurring narration verbs that leaked into
# the rare-lowercase heuristic as false candidates. Still hand-authored facts.
SUPPLEMENT = """
able unable ability abilities information detail details specific general area
areas amount amounts value values total totals average averages quantity number
numbers level levels point points second seconds minute minutes hour hours day
days week weeks month months year years moment moments plus minus non anti semi
extra bonus penalty percent degree degrees increase increased increases increasing
decrease decreased reduce reduced reducing gain gained gaining lose losing loss
mental physical magical natural normal special common rare basic advanced power
powers energy energies force forces strength strengths agility dexterity intelligence
wisdom constitution stamina focus resistance resistances defense defence offense
health healing damage effect effects status ability skill skills spell spells item
items weapon weapons armor armour shield shields tool tools resource resources
material materials system systems process processes method methods result results
option options choice choices decision decisions action actions reaction reactions
movement motions position positions location locations direction directions distance
distances speed speeds size sizes shape shapes color colors sound sounds light
lights shadow shadows fire fires water waters earth earths wind winds ice stone
stones metal metals wood woods leaf leaves branch branches root roots seed seeds
plant plants animal animals beast beasts creature creatures monster monsters human
humans person persons people group groups team teams member members leader leaders
enemy enemies ally allies friend friends family families child children parent
parents woman women man men eye eyes ear ears nose mouth mouths hand hands arm
arms leg legs foot feet head heads face faces hair back backs chest finger fingers
skin body bodies heart hearts mind minds soul souls voice voices word words name
names story stories place places world worlds land lands city cities town towns
village villages house houses home homes room rooms door doors wall walls floor
window windows road roads path paths gate gates field fields forest forests
mountain mountains river rivers lake lakes sea seas sky skies ground grounds
towards toward within without among along across behind beyond beneath around
nearby elsewhere anywhere everywhere nowhere somewhere upward downward forward
backward inward outward really actually finally suddenly quickly slowly clearly
slightly simply merely nearly barely hardly mostly partly fully greatly deeply
highly widely evenly quietly loudly gently roughly firmly steadily gradually
immediately instantly constantly frequently occasionally usually normally
generally typically especially particularly certainly probably possibly perhaps
maybe indeed instead rather quite fairly somewhat entirely completely absolutely
totally exactly precisely roughly nearly almost enough plenty several various
multiple single double triple entire whole partial complete overall began begun
brought thought caught taught fought sought bought sought seemed seeming looked
looking asked asking told telling said saying went gone came coming took taken
gave given made making found finding kept keeping held holding stood standing sat
sitting walked walking ran running turned turning moved moving looked reached
reaching pulled pushed pushing dropped dropping lifted lifting carried carrying
pointed pointing nodded nodding shook shaking smiled smiling frowned staring
watching noticed noticing realized realizing remembered wondered wondering decided
deciding continued continuing started starting stopped stopping tried trying
managed managing placed placing created creating formed forming gathered gathering
increased entered entering appeared appearing continued
arrow arrows target targets vision visions coin coins wolf wolves per getting get
gets got gotten release released releasing releases axe axes prompt prompts sword
swords shield bow bows blade blades knife knives spear spears staff staffs rod rods
kilogram kilograms gram grams meter meters mile miles pound pounds inch inches
foot feet ounce ounces ton tons dozen pair pairs dozen bit bits lot lots piece
pieces part parts side sides edge edges top tops bottom row rows line lines corner
corners center middle end ends beginning start starts finish set sets pack packs
box boxes bag bags basket baskets pot pots cup cups plate plates bowl bowls barrel
gold silver copper bronze iron steel wooden stone leather cloth glass crystal
coin gem gems jewel jewels treasure treasures chest chests vault door key keys lock
locks chain chains rope ropes net nets trap traps cage cages ladder stairs bridge
tower towers castle castles temple temples shrine altar throne cave caves tunnel
tunnels dungeon dungeons camp camps market markets shop shops inn inns tavern
prompt notification menu screen bar meter gauge slot slots tier tiers rank ranks
grade grades class classes race races kind kinds type types form forms sort sorts
""".split()
for w in SUPPLEMENT:
    words.add(w.lower())

# Extra high-frequency function/short words + numbers spelled out.
EXTRA = """
zero one two three four five six seven eight nine ten eleven twelve thirteen
fourteen fifteen sixteen seventeen eighteen nineteen twenty thirty forty fifty
sixty seventy eighty ninety hundred thousand million first second third fourth
fifth sixth seventh eighth ninth tenth i'm i've i'll i'd you're you've you'll
you'd he's she's it's we're we've they're don't didn't doesn't won't wouldn't
can't couldn't shouldn't isn't aren't wasn't weren't haven't hasn't hadn't ain't
that's there's here's what's who's let's o'clock 'em 'tis
""".split()
for w in EXTRA:
    words.add(w.lower())

out = sorted(words)

lines = []
lines.append("// Code generated by gen_commonwords.py; DO NOT EDIT.")
lines.append("")
lines.append("package spelling")
lines.append("")
lines.append("// commonWordsRaw is a modest, hand-assembled list of common English words")
lines.append("// (whitespace-separated, lowercase). Individual common words are facts and a")
lines.append("// plain sorted list of them carries no creative expression, so this list is")
lines.append("// license-safe: it was hand-authored for this filter, not copied from an")
lines.append("// external corpus. It is used ONLY to filter ordinary English words out of the")
lines.append("// rare-repeated-lowercase proper-noun candidate heuristic (ExtractCandidates")
lines.append("// category 4), so a word missing from it only means a slightly noisier")
lines.append("// candidate list, never a correctness problem.")
lines.append("const commonWordsRaw = `" + " ".join(out) + "`")
lines.append("")

import io
buf = io.StringIO()
buf.write("\n".join(lines))
text = buf.getvalue()

# Wrap the long const line for readability is unnecessary; keep single line.
with open("commonwords.go", "w") as f:
    f.write(text)

print("word count:", len(out))
print("bytes:", len(text))
