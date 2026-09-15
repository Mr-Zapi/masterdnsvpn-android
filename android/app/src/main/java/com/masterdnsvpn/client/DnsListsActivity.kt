package com.masterdnsvpn.client

import android.app.AlertDialog
import android.content.Context
import android.content.Intent
import android.graphics.Color
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.view.LayoutInflater
import android.view.View
import android.view.ViewGroup
import android.widget.BaseAdapter
import android.widget.Button
import android.widget.EditText
import android.widget.LinearLayout
import android.widget.ListView
import android.widget.TextView
import android.widget.Toast
import androidx.appcompat.app.AppCompatActivity
import androidx.core.content.ContextCompat
import java.util.concurrent.Executors

/**
 * Manages saved DNS lists: create, rename, duplicate, delete, set active and
 * kick off a public-DNS scan.
 */
class DnsListsActivity : AppCompatActivity() {

    private lateinit var store: DnsListStore
    private lateinit var repo: PublicDnsRepository

    private lateinit var listsView: ListView
    private lateinit var emptyText: TextView
    private lateinit var cacheStatus: TextView
    private lateinit var refreshButton: TextView
    private lateinit var scanButton: Button
    private lateinit var newButton: Button

    private val adapter by lazy { ListAdapter() }
    private val executor = Executors.newSingleThreadExecutor { r -> Thread(r, "dns-lists-io") }
    private val main = Handler(Looper.getMainLooper())
    private val prefs by lazy { getSharedPreferences("masterdnsvpn", Context.MODE_PRIVATE) }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_dns_lists)

        store = DnsListStore.get(this)
        repo = PublicDnsRepository.get(this)

        listsView = findViewById(R.id.listsView)
        emptyText = findViewById(R.id.emptyText)
        cacheStatus = findViewById(R.id.cacheStatus)
        refreshButton = findViewById(R.id.refreshButton)
        scanButton = findViewById(R.id.scanButton)
        newButton = findViewById(R.id.newButton)

        findViewById<TextView>(R.id.backButton).setOnClickListener { finish() }

        listsView.adapter = adapter
        listsView.setOnItemClickListener { _, _, position, _ ->
            val list = adapter.getItem(position)
            store.setActive(list.id)
            refresh()
            Toast.makeText(this, "Active: ${list.name}", Toast.LENGTH_SHORT).show()
        }
        listsView.setOnItemLongClickListener { _, _, position, _ ->
            showListOptions(adapter.getItem(position))
            true
        }

        newButton.setOnClickListener { promptNewList() }
        scanButton.setOnClickListener { openScan() }
        refreshButton.setOnClickListener { refreshPublicList() }
    }

    override fun onResume() {
        super.onResume()
        refresh()
    }

    private fun refresh() {
        adapter.notifyDataSetChanged()
        val hasLists = store.all().isNotEmpty()
        emptyText.visibility = if (hasLists) View.GONE else View.VISIBLE
        updateCacheStatus()
    }

    private fun updateCacheStatus() {
        val meta = repo.cachedMeta()
        cacheStatus.text = if (meta == null) {
            "Merged DNS cache: empty"
        } else {
            "Merged DNS cache: ${meta.total} servers, ${meta.countries.size} countries " +
                "(${ageText(repo.cacheAgeMillis())})"
        }
    }

    private fun ageText(ageMillis: Long): String {
        if (ageMillis < 0) return "unknown"
        val minutes = ageMillis / 60_000
        return when {
            minutes < 1 -> "just now"
            minutes < 60 -> "${minutes}m ago"
            minutes < 1440 -> "${minutes / 60}h ago"
            else -> "${minutes / 1440}d ago"
        }
    }

    private fun promptNewList() {
        val input = EditText(this).apply { hint = "List name" }
        val serversInput = EditText(this).apply {
            hint = "One resolver per line\ne.g. 77.88.8.8:53"
            minLines = 3
        }
        val container = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(48, 16, 48, 0)
            addView(input)
            addView(serversInput)
        }
        AlertDialog.Builder(this)
            .setTitle("New DNS list")
            .setView(container)
            .setPositiveButton("Create") { _, _ ->
                val name = input.text.toString().ifBlank { "New list" }
                val servers = serversInput.text.toString().split('\n', ',', ' ', '\t')
                store.create(name, servers, DnsList.SOURCE_MANUAL)
                refresh()
            }
            .setNegativeButton("Cancel", null)
            .show()
    }

    private fun showListOptions(list: DnsList) {
        val options = arrayOf("Set active", "Rescan", "Rename", "Duplicate", "Delete")
        AlertDialog.Builder(this)
            .setTitle(list.name)
            .setItems(options) { _, which ->
                when (which) {
                    0 -> {
                        store.setActive(list.id)
                        refresh()
                    }
                    1 -> rescan(list)
                    2 -> promptRename(list)
                    3 -> {
                        store.duplicate(list.id)
                        refresh()
                    }
                    4 -> confirmDelete(list)
                }
            }
            .show()
    }

    private fun rescan(list: DnsList) {
        val config = prefs.getString("config_b64", "").orEmpty()
        if (config.isBlank()) {
            Toast.makeText(this, "Paste your base64 config on the main screen first", Toast.LENGTH_LONG).show()
            return
        }
        val intent = Intent(this, ScanActivity::class.java)
        intent.putExtra(ScanActivity.EXTRA_LIST_ID, list.id)
        startActivity(intent)
    }

    private fun promptRename(list: DnsList) {
        val input = EditText(this).apply {
            setText(list.name)
            setSelection(list.name.length)
        }
        val container = LinearLayout(this).apply {
            setPadding(48, 16, 48, 0)
            addView(input)
        }
        AlertDialog.Builder(this)
            .setTitle("Rename list")
            .setView(container)
            .setPositiveButton("Save") { _, _ ->
                store.rename(list.id, input.text.toString())
                refresh()
            }
            .setNegativeButton("Cancel", null)
            .show()
    }

    private fun confirmDelete(list: DnsList) {
        AlertDialog.Builder(this)
            .setTitle("Delete \"${list.name}\"?")
            .setMessage("This removes the saved list. It cannot be undone.")
            .setPositiveButton("Delete") { _, _ ->
                store.delete(list.id)
                refresh()
            }
            .setNegativeButton("Cancel", null)
            .show()
    }

    private fun openScan() {
        val config = prefs.getString("config_b64", "").orEmpty()
        if (config.isBlank()) {
            Toast.makeText(this, "Paste your base64 config on the main screen first", Toast.LENGTH_LONG).show()
            return
        }
        startActivity(Intent(this, ScanActivity::class.java))
    }

    private fun refreshPublicList() {
        refreshButton.isEnabled = false
        cacheStatus.text = "Downloading public DNS list..."
        Toast.makeText(this, "Downloading public DNS list...", Toast.LENGTH_SHORT).show()

        executor.execute {
            val result: Result<PublicDnsRepository.Meta> = try {
                repo.refreshAll()
            } catch (t: Throwable) {
                Result.failure(t)
            }
            main.post {
                refreshButton.isEnabled = true
                result.onSuccess { meta ->
                    Toast.makeText(
                        this,
                        "Merged ${meta.total} servers from ${meta.countries.size} countries",
                        Toast.LENGTH_LONG
                    ).show()
                }.onFailure {
                    val cached = repo.hasCache()
                    Toast.makeText(
                        this,
                        if (cached) "Download failed - using previous cached list" else "Download failed",
                        Toast.LENGTH_LONG
                    ).show()
                }
                refresh()
            }
        }
    }

    private inner class ListAdapter : BaseAdapter() {
        private val items: List<DnsList>
            get() = store.all()

        override fun getCount(): Int = items.size
        override fun getItem(position: Int): DnsList = items[position]
        override fun getItemId(position: Int): Long = items[position].id.hashCode().toLong()

        override fun getView(position: Int, convertView: View?, parent: ViewGroup): View {
            val view = convertView ?: LayoutInflater.from(this@DnsListsActivity)
                .inflate(R.layout.item_dns_list, parent, false)
            val list = getItem(position)

            val mark = view.findViewById<View>(R.id.activeMark)
            val name = view.findViewById<TextView>(R.id.listName)
            val subtitle = view.findViewById<TextView>(R.id.listSubtitle)
            val menu = view.findViewById<TextView>(R.id.menuButton)

            val isActive = list.id == store.activeId()
            mark.setBackgroundColor(
                if (isActive) ContextCompat.getColor(this@DnsListsActivity, R.color.accent) else Color.TRANSPARENT
            )
            name.text = list.name
            subtitle.text = "${list.servers.size} servers \u2022 ${sourceLabel(list.source)}" +
                if (isActive) "  \u2022  ACTIVE" else ""
            menu.setOnClickListener { showListOptions(list) }
            return view
        }
    }

    private fun sourceLabel(source: String): String = when (source) {
        DnsList.SOURCE_SCAN -> "scanned"
        DnsList.SOURCE_PUBLIC -> "public list"
        DnsList.SOURCE_LEGACY -> "imported"
        else -> "manual"
    }

    override fun onDestroy() {
        executor.shutdownNow()
        super.onDestroy()
    }
}
