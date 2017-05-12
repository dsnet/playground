// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE.md file.

var defaultID = 1; // Must match the value in snippets.go

function escapeHTML(str) {
	var buf = [];
	for (var i = str.length-1; i >= 0; i--) {
		buf.unshift(["&#", str[i].charCodeAt(), ";"].join(""));
	}
	return buf.join("");
}

var outputOffset = 0;
var outputLimit = 1<<18; // 256KiB
function clearOutput() {
	var node = document.getElementById("outputPane");
	while (node.firstChild) {
		node.removeChild(node.firstChild);
	}
	outputOffset = 0;
}

// doAutoscroll scrolls the page automatically if viewpoint is already really
// close to the bottom of the page.
// This calls f, where f may mutate the outputPane in some way.
function doAutoscroll(f) {
	var op = document.getElementById("outputPane")
	var autoscroll = (op.scrollTop > 0) &&
		(op.scrollHeight - (op.clientHeight + op.scrollTop) < 10);
	f();
	if (autoscroll) {
		op.scrollTop = op.scrollHeight;
	}
}

// appendOutput displays msg in the output pane as either stdout, stderr, or
// as a status update.
function appendOutput(msg, cls) {
	doAutoscroll(function() {
		// Ensure that the output does not exceed some size.
		// This is important to prevent an accidental infinite loop from
		// utterly locking up the client's browser.
		if (cls == "stdout" || cls == "stderr") {
			if (outputOffset > outputLimit) return; // Only print error once
			outputOffset += msg.length;
			if (outputOffset > outputLimit) {
				handleStop();
				msg = "Terminating program... (output exceeded " + outputLimit.toString() + " bytes)\n";
				cls = "status";
			}
		}

		// Always ensure status updates are on a new line.
		if (cls == "status") {
			var node = document.getElementById("outputPane").lastChild;
			if (node && node.innerHTML.substr(-1) != "\n") {
				msg = "\n" + msg;
			}
		}

		var node = document.createElement("span");
		node.className = cls;
		node.innerHTML = escapeHTML(msg);
		document.getElementById("outputPane").appendChild(node);
	});
}

// snippetDB allows performing CRUD operations on the snippet database.
var snippetDB = {
	"queryByName": function(q) {
		var req = new XMLHttpRequest();
		q = (q) ? "&query="+encodeURIComponent(JSON.stringify(q)) : "";
		req.open("GET", "/snippets?queryBy=name&limit=100" + q, false);
		req.send();
		switch (req.status) {
		case 200:
			var ss = JSON.parse(req.responseText);
			return {"snippets": ss || [], "ok": true};
		default:
			var msg = "Status " + req.status.toString() + ": " + req.responseText;
			swal("Something went wrong:", msg, "error");
			return {"ok": false};
		}
	},
	"queryByModified": function(q) {
		var req = new XMLHttpRequest();
		q = (q) ? "&query="+encodeURIComponent(JSON.stringify(q)) : "";
		req.open("GET", "/snippets?queryBy=modified&limit=10" + q, false);
		req.send();
		switch (req.status) {
		case 200:
			var ss = JSON.parse(req.responseText);
			return {"snippets": ss || [], "ok": true};
		default:
			var msg = "Status " + req.status.toString() + ": " + req.responseText;
			swal("Something went wrong:", msg, "error");
			return {"ok": false};
		}
	},
	"create": function(s) {
		var req = new XMLHttpRequest();
		req.open("POST", "/snippets", false);
		req.send(JSON.stringify(s));
		switch (req.status) {
		case 200:
			var s = JSON.parse(req.responseText);
			return {"snippet": s, "ok": true};
		default:
			var msg = "Status " + req.status.toString() + ": " + req.responseText;
			swal("Something went wrong:", msg, "error");
			return {"ok": false};
		}
	},
	"retrieve": function(id) {
		var req = new XMLHttpRequest();
		req.open("GET", "/snippets/"+id.toString(), false);
		req.send();
		switch (req.status) {
		case 200:
			var s = JSON.parse(req.responseText);
			s.code = s.code || "";
			return {"snippet": s, "ok": true};
		case 404:
			return {"ok": false, "notFound": true};
		default:
			var msg = "Status " + req.status.toString() + ": " + req.responseText;
			swal("Something went wrong:", msg, "error");
			return {"ok": false};
		}
	},
	"update": function(s) {
		var req = new XMLHttpRequest();
		req.open("PUT", "/snippets/" + s.id.toString(), false);
		req.send(JSON.stringify(s));
		switch (req.status) {
		case 200:
			return {"ok": true};
		default:
			var msg = "Status " + req.status.toString() + ": " + req.responseText;
			swal("Something went wrong:", msg, "error");
			return {"ok": false};
		}
	},
	"delete": function(id) {
		var req = new XMLHttpRequest();
		req.open("DELETE", "/snippets/" + id.toString(), false);
		req.send();
		switch (req.status) {
		case 200:
			return {"ok": true};
		default:
			var msg = "Status " + req.status.toString() + ": " + req.responseText;
			swal("Something went wrong:", msg, "error");
			return {"ok": false};
		}
	},
}

var editor;
function setupCodeMirror() {
	editor = CodeMirror.fromTextArea(document.getElementById("codeBox"), {
		theme: "play",
		lineNumbers: true,
		styleActiveLine: true,
		tabSize: 4,
		indentUnit: 4,
		indentWithTabs: true,
		autofocus: true,
		gutters: ["CodeMirror-linenumbers", "issues"],
	});
	editor.setSize("100%", "70%");
}

// setupWebsocket opens a connection to the server over websocket and handles
// all actions that the server will send the client.
var websock;
var running = false; // Are we currently executing something on the server?
var connected = false;
function setupWebsocket() {
	var scheme = (window.location.protocol === "https:") ? "wss:" : "ws:";
	var url = scheme + "//" + window.location.host + "/websocket";
	websock = new WebSocket(url);

	websock.onopen = function() {
		clearOutput();
		connected = true;
	}

	// Register callback for receiving messages from the server.
	websock.onmessage = function(event) {
		var msg = JSON.parse(event.data);
		processMessage(msg);
	}

	// Register callback to automatically reconnect.
	websock.onclose = function() {
		if (connected) {
			appendOutput("Disconnected from server...\n", "status");
		}
		connected = false;
		setTimeout(function() { setupWebsocket() }, 1000);
	}
}

// processMessage handles event messages coming from the server.
function processMessage(msg) {
	switch (msg.action) {
	case "clearOutput":
		clearOutput();
		break;
	case "markLines":
		var lines = JSON.parse(msg.data);
		for (var i = 0; i < lines.length; i++) {
			var div = document.createElement("div");
			div.className = "gutterMarker";
			div.innerHTML = "&nbsp;";
			editor.setGutterMarker(lines[i]-1, "issues", div);
		}
		break;
	case "statusStarted":
		running = true;
		document.getElementById("buttonStop").disabled = false;
		break;
	case "statusStopped":
		running = false;
		document.getElementById("buttonStop").disabled = true;
		break;
	case "statusUpdate":
		appendOutput(msg.data, "status");
		break;
	case "appendStdout":
		appendOutput(msg.data, "stdout");
		break;
	case "appendStderr":
		appendOutput(msg.data, "stderr");
		break;
	case "reportProfile":
		doAutoscroll(function() {
			var report = JSON.parse(msg.data);
			var a = document.createElement("a");
			a.href = "/dynamic/" + report.id;
			a.target = "_blank";
			a.className = "status";
			a.appendChild(document.createTextNode(report.name));

			var span = document.createElement("span");
			span.className = "status";
			span.appendChild(document.createTextNode("\tGenerated report: "));
			span.appendChild(a);
			span.appendChild(document.createTextNode("\n"));
			document.getElementById("outputPane").appendChild(span);
		});
		break;
	case "format":
		if (editor.getValue() != msg.data) {
			// When formatting, try to preserve the original cursor.
			var cursor = editor.getCursor();
			var top = editor.getScrollInfo().top;
			editor.setValue(msg.data);
			editor.setCursor({"line": cursor.line, "ch": cursor.ch});
			editor.scrollTo(0, top);
		}
		break;
	default:
		console.log("unknown message: " + msg.action);
	}
}

var snippet = {};
function loadSnippet(id) {
	if (id != null && snippet.id == id) return true;

	var ret = snippetDB.retrieve(id || defaultID);
	if (!ret.ok) return false;

	if (id == null) {
		// Load the default snippet as a new template.
		delete ret.snippet.id;
		delete ret.snippet.name;
		window.history.pushState(null, "", "/");
		document.getElementById("snippetName").value = "";
	} else {
		window.history.pushState(null, "", "/" + id.toString());
		document.getElementById("snippetName").value = ret.snippet.name;
	}
	document.getElementById("buttonDelete").disabled = (id == null || id == defaultID);

	editor.setValue(ret.snippet.code);
	editor.clearGutter("issues");
	editor.clearHistory();
	clearOutput();
	if (running) {
		websock.send(JSON.stringify({action: "stop"}));
		websock.send(JSON.stringify({action: "clearOutput"}));
	}

	snippet = ret.snippet;
	return true;
}

function saveSnippet() {
	if (snippet.id == null) {
		return true;
	}
	var name = document.getElementById("snippetName").value;
	var code = editor.getValue();
	if (snippet.name == name && snippet.code == code) {
		return true; // No change, so return success
	}

	var ret = snippetDB.update({id: snippet.id, name: name, code: code});
	if (!ret.ok) return false;
	snippet.name = name;
	snippet.code = code;

	document.getElementById("buttonDelete").disabled = (snippet.id == null || snippet.id == defaultID);
	window.history.pushState(null, "", "/" + snippet.id.toString());
	return true;
}

function reloadListing() {
	var ret;
	var val = document.getElementById("snippetSearch").value;
	var hideID = null;
	if (val == "") {
		ret = snippetDB.queryByModified();
		if (!ret.ok) return false;
		hideID = defaultID;
	} else {
		ret = snippetDB.queryByName({name: val});
		if (!ret.ok) return false;
	}

	// Clear out all previous listings.
	var node = document.getElementById("snippetListing");
	while (node.firstChild) {
		node.removeChild(node.firstChild);
	}

	// If there are no snippets, indicate such is so.
	if (ret.snippets.length == 0) {
		var li = document.createElement("li");
		li.appendChild(document.createTextNode("No results to show"));
		li.className = "listEmpty";
		document.getElementById("snippetListing").appendChild(li);
		return true;
	}

	// Append an list item for every snippet.
	appendListing(ret.snippets, hideID);
	return moreListing();
}

// refreshListing iterates through all of the list items setting the proper
// class names for each given item.
function refreshListing() {
	var ul = document.getElementById("snippetListing");
	for (var i = 0; i < ul.childNodes.length; i++) {
		var li = ul.childNodes[i];
		li.className = "listItem";
		var id = parseInt(li.dataset.id);
		if (id == snippet.id) {
			li.removeChild(li.firstChild);
			li.appendChild(document.createTextNode(snippet.name));
			li.className += " listItemSelect";
		}
		if (id == defaultID) li.className += " listItemDefault";
	}
}

function moreListing() {
	// If the list mode is by name search, then stop.
	var val = document.getElementById("snippetSearch").value;
	if (val != "") return true;

	// If the number of children is zero, then load the first set.
	var ul = document.getElementById("snippetListing");
	if (ul.childNodes.length == 0) {
		if (!reloadListing()) return false;
	}

	// Keep appending elements until either there are no more or we have enough
	// to fill the entire listing.
	while (ul.scrollTop + ul.clientHeight == ul.scrollHeight) {
		if (ul.lastChild.dataset.id == null) return true;
		var id = parseInt(ul.lastChild.dataset.id);
		var modified = ul.lastChild.dataset.modified;
		ret = snippetDB.queryByModified({id: id, modified: modified});
		if (!ret.ok) return false;
		if (ret.snippets.length == 0) return true;
		appendListing(ret.snippets, defaultID);
	}
	return true;
}

// appendListing appends a set of list items using the provided snippets.
function appendListing(snippets, hideID) {
	var ul = document.getElementById("snippetListing");
	for (var i = 0; i < snippets.length; i++) {
		var s = snippets[i];
		var li = document.createElement("li");
		li.onclick = handleLoad;
		li.appendChild(document.createTextNode(s.name));
		li.dataset.id = s.id;
		li.dataset.modified = s.modified;
		li.className = "listItem";
		if (s.id == snippet.id) li.className += " listItemSelect";
		if (s.id == defaultID)  li.className += " listItemDefault";
		if (s.id == hideID)     li.style.display = "none";
		ul.appendChild(li);
	}
}

function handleLoad(event) {
	var id = parseInt(event.target.dataset.id);

	// Avoid prompt if there is no code that may be lost.
	if (snippet.id != null || snippet.code == editor.getValue()) {
		if (!saveSnippet()) return;
		if (!loadSnippet(id)) return;
		refreshListing();
		return;
	}

	// Show warning prompt for possible lost work.
	swal({
		title: "Load Snippet",
		text: "There are unsaved changes on the current snippet.\
			Those changes will be lost if another snippet is loaded. Proceed?",
		type: "warning",
		showCancelButton: true,
		confirmButtonText: "Yes",
		confirmButtonClass: "redButton",
	}).then(function() {
		if (!saveSnippet()) return false;
		if (!loadSnippet(id)) return false;
		refreshListing();
	}, function() {});
}

function handleRename() {
	var nameBox = document.getElementById("snippetName");
	var name = nameBox.value;
	if (snippet.id == defaultID && name != snippet.name) {
		nameBox.value = snippet.name;
		swal("Invalid Operation", "Cannot change name of the default snippet.", "error");
		return;
	}
	if (name == "" && snippet.name != null) {
		nameBox.value = snippet.name;
		swal("Invalid Operation", "Cannot use an empty name.", "error");
		return;
	}
	if (name == "" || name == snippet.name) {
		return;
	}

	if (snippet.id != null) {
		if (!saveSnippet()) return;
		refreshListing();
	} else {
		// Rename on non-existent snippet is an implied create.
		var ret = snippetDB.create({name: name});
		if (!ret.ok) return;
		snippet = {id: ret.snippet.id}
		if (!saveSnippet()) return; // This will store the actual code
		reloadListing();
	}
}

function handleNew() {
	swal({
		title: "New Snippet",
		text: "Provide a new name:",
		input: "text",
		showCancelButton: true,
		confirmButtonClass: "blueButton",
		preConfirm: function(name) {
			return new Promise(function(resolve, reject) {
				(name == "") ? reject("Name cannot be empty!") : resolve();
			});
		},
	}).then(function(name) {
		if (!saveSnippet()) return false;
		var ret = snippetDB.retrieve(defaultID);
		if (!ret.ok) return false;
		var ret = snippetDB.create({name: name, code: ret.snippet.code});
		if (!ret.ok) return false;
		if (!loadSnippet(ret.snippet.id)) return false;
		if (!reloadListing()) return false;
	}, function() {});
}

function handleSave() {
	swal({
		title: "Save Snippet",
		text: "Provide a new name:",
		input: "text",
		showCancelButton: true,
		confirmButtonClass: "blueButton",
		preConfirm: function(name) {
			return new Promise(function(resolve, reject) {
				(name == "") ? reject("Name cannot be empty!") : resolve();
			});
		},
	}).then(function(name) {
		if (!saveSnippet()) return false;
		var ret = snippetDB.create({name: name, code: editor.getValue()});
		if (!ret.ok) return false;
		if (!loadSnippet(ret.snippet.id)) return false;
		if (!reloadListing()) return false;
	}, function() {});
}

function handleDelete() {
	if (snippet.id == null || snippet.id == defaultID) {
		swal("Invalid Operation", "Cannot delete an unsaved or default snippet.", "error");
		return;
	}

	swal({
		title: "Delete Snippet",
		text: "This will permanently delete the snippet. Proceed?",
		type: "warning",
		showCancelButton: true,
		confirmButtonText: "Yes",
		confirmButtonClass: "redButton",
	}).then(function(){
		var ret = snippetDB.delete(snippet.id);
		if (!ret.ok) return;
		snippet = {}; // Clear the snippet in case the load fails
		if (!loadSnippet(null)) return false;
		if (!reloadListing()) return false;
	}, function() {});
}

function handleRun() {
	running = true;
	editor.clearGutter("issues");
	var msg = {action: "run", data: editor.getValue()};
	websock.send(JSON.stringify(msg));
}

function handleFormat() {
	running = true;
	editor.clearGutter("issues");
	var msg = {action: "format", data: editor.getValue()};
	websock.send(JSON.stringify(msg));
}

function handleStop() {
	var msg = {action: "stop"};
	websock.send(JSON.stringify(msg));
}

function handleHelp() {
	var msg = "";
	msg += "<div style=\"text-align: left; overflow-y: auto; max-height: 20em;\">";
	msg += "<b>Playground</b> is a web application that executes arbitrary Go code locally.\
		This tool provides the ability to save and load various code snippets,\
		the ability to run Go code using arbitrary third-party packages,\
		and the ability to run tests and benchmarks.";
	msg += "<br>";
	msg += "<br>";
	msg += "The panel on the left shows a listing of saved code snippets and clicking on an entry loads that snippet.\
		Any modifications to a given snippet is automatically saved.\
		New snippets can be created by clicking the <code>New</code> or <code>Save As</code> buttons.\
		When creating new snippets, they will be initialized with some default code.\
		This default can be changed by searching for and directly altering the snippet labeled <code>\"Default snippet\"</code>.";
	msg += "<br>";
	msg += "<br>";
	msg += "Each code snippet is an individual Go program and will be executed as either an executable or a test suite.\
		The presence of a <code>main</code> function or any <code>Test</code> or\
		<code>Benchmark</code> functions determine what type of program it is.\
		These functions cannot be mixed; that is, only one type or the other may exist within the snippet.\
		Lastly, all snippets must be within the <code>main</code> package.";
	msg += "<br>";
	msg += "<br>";
	msg += "In order to provide finer grained control over the build and run environment,\
		certain magical comments can be placed at the top of the source code.\
		These comments take the form <code>//playground:tag arg1 arg2</code>,\
		where <code>tag</code> is some identifier followed by a list of arguments.\
		For example:";
	msg += "<pre style=\"padding-left: 1em;\">";
	msg += "//playground:goversions go1.4 go1.5 go1.6\n//playground:buildargs -race\n";
	msg += "//playground:execargs -test.v -test.run Encode\n//playground:pprof cpu mem\n";
	msg += "</pre>";
	msg += "These magic comments allow the playground to build and execute the program with specific parameters.";
	msg += "</div>";
	swal({title: "Playground Help", html: msg, confirmButtonClass: "blueButton"});
}

// These set of callbacks provide hot-keys to all of the functionality that
// is triggered by UI button presses.
document.onkeydown = function(event) {
	if (event.code == "Enter" || event.keyCode == 13) { // Enter Key
		if (event.shiftKey) { // Run snippet
			handleRun();    event.preventDefault(); return;
		} else if (event.altKey || event.ctrlKey) { // Format snippet
			handleFormat(); event.preventDefault(); return;
		}
	}
	if (event.altKey || event.ctrlKey) {
		switch (event.keyCode) {
		case 75: handleStop();   event.preventDefault(); return; // H
		case 72: handleHelp();   event.preventDefault(); return; // K
		case 78: handleNew();    event.preventDefault(); return; // N
		case 83: handleSave();   event.preventDefault(); return; // S
		case 46: handleDelete(); event.preventDefault(); return; // Del
		}
	}

	// TODO(dsnet): Allow the client to send stdin to the server?
	if (event.target.id == "outputPane") {}
}
document.onkeyup = function(event) {
	switch (event.target.id) {
	case "snippetName":
		if (event.keyCode == 27 || event.keyCode == 13) { // Escape or enter key
			handleRename();
			event.target.blur();
		}
		return;
	case "snippetSearch":
		if (event.keyCode == 27) { // Escape key
			event.target.value = "";
			event.target.blur();
		}
		reloadListing();
		return;
	case "outputPane":
		if (event.keyCode == 35) { // End key
			var op = document.getElementById("outputPane");
			op.scrollTop = op.scrollHeight;
		}
		return;
	}
}

// These set of callbacks ensures that the listing of snippets have the proper
// number of snippet items loaded. That is, if there is extra display space,
// automatically fetch for more snippets.
window.onresize = moreListing;
document.getElementById("snippetListing").onscroll = moreListing;

// These set of callbacks allows the user to resize the code mirror and output
// panes relative to each other.
var draggingH = false, draggingV = false;
document.getElementById("dragBarH").onmousedown = function() { draggingH = true; }
document.getElementById("dragBarV").onmousedown = function() { draggingV = true; }
document.onmouseup = function() { draggingH = false; draggingV = false; }
document.onmousemove = function(event) {
	if (draggingH) {
		event.preventDefault();
		var cm = document.getElementsByClassName("CodeMirror")[0];
		var height = event.clientY - cm.getBoundingClientRect().top;
		cm.style.height = (height > 0) ? height : 0;
	}
	if (draggingV) {
		event.preventDefault();
		var width = event.clientX;
		width = (width > 240) ? width : 240;
		width = (width > window.innerWidth-270) ? window.innerWidth-270 : width;
		document.getElementById("leftPane").style.width = width;
		document.getElementById("dragBarV").style.left = width;
		document.getElementById("rightPane").style.marginLeft = width+10;
	}
}

function init() {
	var id = null;
	if (window.location.pathname != "/") {
		var arr = window.location.pathname.split("/");
		id = parseInt(arr[arr.length-1], 10);
		if (isNaN(id)) {
			id = null;
		}
	}

	setupCodeMirror();
	setupWebsocket();
	if (!loadSnippet(id)) {
		loadSnippet(null);
	}
	moreListing();

	// Register callbacks for saving the snippet.
	setInterval(saveSnippet, 30000); // Every 30 seconds
	window.onbeforeunload = function() { saveSnippet(); } // Upon page closure
}
init();
