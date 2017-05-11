// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE.md file.

function loginPrompt() {
	swal({
		title: "Authentication Required",
		text: "Please provide the login password:",
		input: "password",
		confirmButtonText: "Submit",
		confirmButtonClass: "blueButton",
		showCancelButton: false,
		allowOutsideClick: false,
		allowEscapeKey: false,
		preConfirm: function(password) {
			return new Promise(function(resolve, reject) {
				// Verify that the authentication token.
				var req = new XMLHttpRequest();
				req.open("POST", "/login", false);
				req.send(password);
				switch (req.status) {
				case 200:
					resolve();
					return;
				default:
					reject("The provided password did not match server records!");
					return;
				}
			});
		},
	}).then(function(password) {
		setTimeout(function() { location.reload(); }, 1000);
		swal({
			title: "Success",
			text: "You are now logged in!",
			type: "success",
			confirmButtonClass: "blueButton",
		}).then(function() {
			location.reload();
		}, function() {
			location.reload();
		});
	}, function() {});
}
loginPrompt();
